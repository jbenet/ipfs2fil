package gitarchive

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// Kind names a git archive record, the root of every CAR this package writes.
const Kind = "git-archive"

// RecordVersion is the record layout this package writes and reads.
const RecordVersion = 1

// Link is a CID in DAG-CBOR (tag 42).
type Link struct{ cid.Cid }

func (l Link) MarshalCBOR() ([]byte, error) {
	if !l.Defined() {
		return nil, errors.New("undefined link")
	}
	return cbor.Marshal(cbor.Tag{Number: 42, Content: append([]byte{0}, l.Bytes()...)})
}

func (l *Link) UnmarshalCBOR(b []byte) error {
	var t cbor.Tag
	if err := cbor.Unmarshal(b, &t); err != nil {
		return err
	}
	raw, ok := t.Content.([]byte)
	if t.Number != 42 || !ok || len(raw) < 2 || raw[0] != 0 {
		return errors.New("not a DAG-CBOR link")
	}
	c, err := cid.Cast(raw[1:])
	if err != nil {
		return err
	}
	l.Cid = c
	return nil
}

// Record is the root of a git archive snapshot. A full archive has no Prev;
// a delta's Prev is the snapshot it builds on, and its packs hold only
// objects that snapshot's refs did not reach.
type Record struct {
	Kind         string            `cbor:"kind"`
	Version      int               `cbor:"version"`
	Prev         *Link             `cbor:"prev,omitempty"`
	ObjectFormat string            `cbor:"objectFormat"`
	Head         string            `cbor:"head,omitempty"`
	Refs         []Ref             `cbor:"refs"`
	Shallow      []string          `cbor:"shallow,omitempty"`
	Remotes      []Remote          `cbor:"remotes,omitempty"`
	Packs        []Pack            `cbor:"packs,omitempty"`
	Log          []Link            `cbor:"log,omitempty"`
	Commits      int               `cbor:"commits,omitempty"` // commits in this snapshot's log
	Instructions map[string]string `cbor:"instructions,omitempty"`
	// Settings are the non-default settings the snapshot was made with,
	// from options and the repo's ConfigFile, such as archive.tips.
	Settings map[string]string `cbor:"settings,omitempty"`
	Tips     []Tip             `cbor:"tips,omitempty"`
	// Submodules are the submodules at HEAD, each with its own archive's
	// record when they were archived.
	Submodules []Submodule `cbor:"submodules,omitempty"`
	// LFS lists the Git LFS objects this snapshot adds, in chunks;
	// LFSObjects counts them.
	LFS        []Link `cbor:"lfs,omitempty"`
	LFSObjects int    `cbor:"lfsObjects,omitempty"`
	Tool       string `cbor:"tool,omitempty"`
}

// Pack is one stored pack: the pack bytes and its .idx, each a UnixFS file.
type Pack struct {
	Pack    Link  `cbor:"pack"`
	Idx     Link  `cbor:"idx"`
	Objects int   `cbor:"objects"`
	Size    int64 `cbor:"size"`
	Thin    bool  `cbor:"thin,omitempty"`
}

// Commit is one commit in the log: enough for a history page without
// reading packs.
type Commit struct {
	ID        string   `cbor:"id"`
	Tree      string   `cbor:"tree"`
	Parents   []string `cbor:"parents,omitempty"`
	Author    Sig      `cbor:"author"`
	Committer Sig      `cbor:"committer"`
	Subject   string   `cbor:"subject"`
	Signed    bool     `cbor:"signed,omitempty"`
}

// Sig is a commit's author or committer line.
type Sig struct {
	Name  string `cbor:"name"`
	Email string `cbor:"email"`
	Time  int64  `cbor:"time"`
	TZ    string `cbor:"tz"`
}

// LogChunk is one block of the commit log.
type LogChunk struct {
	Commits []Commit `cbor:"commits"`
}

var encMode, _ = cbor.CanonicalEncOptions().EncMode()

var decMode, _ = cbor.DecOptions{
	MaxNestedLevels:  32,
	MaxArrayElements: 1 << 20,
	MaxMapPairs:      1 << 16,
	UTF8:             cbor.UTF8RejectInvalid,
	DupMapKey:        cbor.DupMapKeyEnforcedAPF,
}.DecMode()

// encodeBlock makes a DAG-CBOR block.
func encodeBlock(v any) (cid.Cid, []byte, error) {
	b, err := encMode.Marshal(v)
	if err != nil {
		return cid.Undef, nil, err
	}
	c, err := cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}.Sum(b)
	return c, b, err
}

// DecodeRecord parses and checks a record block.
func DecodeRecord(b []byte) (*Record, error) {
	var r Record
	if err := decMode.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("git archive record: %w", err)
	}
	if r.Kind != Kind {
		return nil, fmt.Errorf("not a git archive record (kind %q)", r.Kind)
	}
	if r.Version != RecordVersion {
		return nil, fmt.Errorf("git archive record version %d; this build reads %d", r.Version, RecordVersion)
	}
	return &r, r.check()
}

// check rejects a record restore could not use safely.
func (r *Record) check() error {
	f, err := ParseFormat(r.ObjectFormat)
	if err != nil {
		return err
	}
	for _, ref := range r.Refs {
		if !strings.HasPrefix(ref.Name, "refs/") || strings.ContainsAny(ref.Name, "\x00\n") {
			return fmt.Errorf("record names ref %q, outside refs/", ref.Name)
		}
		if !isID(ref.ID, f) || (ref.Peeled != "" && !isID(ref.Peeled, f)) {
			return fmt.Errorf("ref %s has a bad object id", ref.Name)
		}
	}
	if r.Head != "" && !isID(r.Head, f) && (!strings.HasPrefix(r.Head, "refs/") || strings.ContainsAny(r.Head, "\x00\n")) {
		return fmt.Errorf("record's HEAD %q is neither a ref nor an object id", r.Head)
	}
	for _, s := range r.Shallow {
		if !isID(s, f) {
			return fmt.Errorf("record's shallow list has a bad id %q", s)
		}
	}
	for _, t := range r.Tips {
		if t.Ref != "HEAD" && (!strings.HasPrefix(t.Ref, "refs/") || strings.ContainsAny(t.Ref, "\x00\n")) {
			return fmt.Errorf("record has a tip folder for %q, outside refs/", t.Ref)
		}
		if !isID(t.Commit, f) || !isID(t.Tree, f) || !t.Root.Defined() || t.Root.Type() != cid.DagProtobuf {
			return fmt.Errorf("tip folder of %s is malformed", t.Ref)
		}
	}
	for _, l := range r.LFS {
		if l.Type() != cid.DagCBOR {
			return errors.New("record's LFS list is not DAG-CBOR")
		}
	}
	for _, s := range r.Submodules {
		if !isID(s.Commit, f) || strings.ContainsAny(s.Path, "\x00\n") {
			return fmt.Errorf("submodule %q is malformed", s.Path)
		}
		if s.Record != nil && (!safeSubmoduleName(s.Name) || s.Record.Type() != cid.DagCBOR) {
			return fmt.Errorf("submodule %q has an unsafe name or a bad record link", s.Name)
		}
	}
	return nil
}

func isID(s string, f Format) bool {
	if len(s) != 2*f.Size {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// parseCommit reads a commit object's header and subject.
func parseCommit(id []byte, data []byte) Commit {
	c := Commit{ID: hex.EncodeToString(id)}
	hdr, msg, _ := bytes.Cut(data, []byte("\n\n"))
	lines := bytes.Split(hdr, []byte("\n"))
	for _, l := range lines {
		k, v, _ := bytes.Cut(l, []byte(" "))
		switch string(k) {
		case "tree":
			c.Tree = string(v)
		case "parent":
			c.Parents = append(c.Parents, string(v))
		case "author":
			c.Author = parseSig(v)
		case "committer":
			c.Committer = parseSig(v)
		case "gpgsig", "gpgsig-sha256":
			c.Signed = true
		}
	}
	subj, _, _ := bytes.Cut(msg, []byte("\n"))
	if len(subj) > 1000 {
		subj = subj[:1000]
	}
	c.Subject = text(subj)
	return c
}

// parseSig reads "Name <email> 1700000000 +0100".
func parseSig(b []byte) Sig {
	var s Sig
	lt := bytes.LastIndexByte(b, '<')
	gt := bytes.LastIndexByte(b, '>')
	if lt < 0 || gt < lt {
		s.Name = text(b)
		return s
	}
	s.Name = text(bytes.TrimSpace(b[:lt]))
	s.Email = text(b[lt+1 : gt])
	f := bytes.Fields(b[gt+1:])
	if len(f) > 0 {
		s.Time, _ = strconv.ParseInt(string(f[0]), 10, 64)
	}
	if len(f) > 1 {
		s.TZ = text(f[1])
	}
	return s
}

// text makes bytes safe for a DAG-CBOR string. Commit metadata is usually
// UTF-8; older commits in other encodings get replacement characters here
// (the exact bytes stay in the pack).
func text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return strings.ToValidUTF8(string(b), "�")
}
