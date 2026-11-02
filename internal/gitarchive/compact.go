package gitarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

// Compaction modes, for Options.Compact.
const (
	// CompactAuto writes a compacted snapshot instead of a delta once the
	// deltas since the last full pack have grown: CompactAfter thin packs
	// already, or thin packs adding up to more than half the full pack.
	CompactAuto = "auto"
	// CompactNow compacts this run, even when nothing changed.
	CompactNow = "now"
	// CompactNever always writes a delta.
	CompactNever = "never"
)

// DefaultCompactAfter is how many thin packs CompactAuto lets pile up:
// the run after the 50th compacts.
const DefaultCompactAfter = 50

// ParseCompact checks a compaction mode; "" is CompactAuto.
func ParseCompact(s string) (string, error) {
	switch s {
	case "", CompactAuto:
		return CompactAuto, nil
	case CompactNow, CompactNever:
		return s, nil
	}
	return "", fmt.Errorf("compact %q: want auto, now or never", s)
}

// compactPlan is a decision to write a compacted snapshot.
type compactPlan struct {
	chain []Snapshot // the last full snapshot up to Prev, oldest first
	why   string
	now   bool // asked for, so even an unchanged repo gets one
}

// planCompaction decides whether this run writes a compacted snapshot
// instead of a delta. A note says why a wanted compaction cannot happen.
func planCompaction(o Options) (*compactPlan, string, error) {
	mode, err := ParseCompact(o.Compact)
	if err != nil {
		return nil, "", err
	}
	if o.Prev == nil || mode == CompactNever {
		return nil, "", nil
	}
	if o.Get == nil {
		if mode == CompactNow {
			return nil, "", errors.New("compacting needs the earlier snapshots' blocks")
		}
		return nil, "", nil
	}
	chain, err := Chain(o.Get, o.PrevRoot)
	if err != nil {
		if mode == CompactNow {
			return nil, "", fmt.Errorf("compacting: %w", err)
		}
		return nil, fmt.Sprintf("not compacting: the earlier snapshots do not read (%v)", err), nil
	}
	var full, thinBytes int64
	thin := 0
	for i, s := range chain {
		for _, p := range s.Record.Packs {
			if i == 0 {
				full += p.Size
			} else {
				thinBytes += p.Size
			}
		}
		if i > 0 && len(s.Record.Packs) > 0 {
			thin++
		}
	}
	after := o.CompactAfter
	if after <= 0 {
		after = DefaultCompactAfter
	}
	p := &compactPlan{chain: chain}
	switch {
	case mode == CompactNow:
		p.why, p.now = "asked for", true
	case thin >= after:
		p.why = fmt.Sprintf("%d thin packs since the last full pack", thin)
	case thinBytes*2 > full:
		p.why = fmt.Sprintf("thin packs since the last full pack come to %d bytes, more than half its %d", thinBytes, full)
	default:
		return nil, "", nil
	}
	return p, "", nil
}

// entryOf is a snapshot's state, for a compacted snapshot's history.
func entryOf(s Snapshot) HistoryEntry {
	r := s.Record
	e := HistoryEntry{Root: s.Root.String(), Head: r.Head, Refs: r.Refs, Shallow: r.Shallow, Remotes: r.Remotes, Commits: r.Commits}
	for _, m := range r.Submodules {
		m.Record = nil
		e.Submodules = append(e.Submodules, m)
	}
	if e.Refs == nil {
		e.Refs = []Ref{}
	}
	return e
}

// history stores the compacted snapshot's history: the last full
// snapshot's own history, carried over block for block, then one entry for
// each snapshot in the chain.
func (p *compactPlan) history(get BlockGetter, put BlockSink) ([]Link, []HistoryEntry, error) {
	var links []Link
	var entries []HistoryEntry
	seen := map[string]bool{}
	for _, l := range p.chain[0].Record.History {
		if seen[l.KeyString()] {
			continue
		}
		seen[l.KeyString()] = true
		b, err := get(l.Cid)
		if err != nil {
			return nil, nil, fmt.Errorf("history: %w", err)
		}
		e, err := decodeEntry(b)
		if err != nil {
			return nil, nil, err
		}
		if err := put(l.Cid, b); err != nil {
			return nil, nil, err
		}
		links, entries = append(links, l), append(entries, *e)
	}
	for _, s := range p.chain {
		e := entryOf(s)
		c, b, err := encodeBlock(e)
		if err != nil {
			return nil, nil, err
		}
		if err := put(c, b); err != nil {
			return nil, nil, err
		}
		links, entries = append(links, Link{c}), append(entries, e)
	}
	if len(links) > maxChain {
		return nil, nil, fmt.Errorf("history would be longer than %d snapshots", maxChain)
	}
	return links, entries, nil
}

func decodeEntry(b []byte) (*HistoryEntry, error) {
	var e HistoryEntry
	if err := decMode.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("history entry: %w", err)
	}
	if _, err := cid.Decode(e.Root); err != nil {
		return nil, fmt.Errorf("history entry names a bad root %q", e.Root)
	}
	return &e, nil
}

// History reads a compacted snapshot's history, oldest first. A block
// named twice is read once.
func History(get BlockGetter, r *Record) ([]HistoryEntry, error) {
	var out []HistoryEntry
	seen := map[string]bool{}
	for _, l := range r.History {
		if seen[l.KeyString()] {
			continue
		}
		seen[l.KeyString()] = true
		b, err := get(l.Cid)
		if err != nil {
			return out, err
		}
		e, err := decodeEntry(b)
		if err != nil {
			return out, err
		}
		out = append(out, *e)
	}
	return out, nil
}

// compactTips is every object the current refs and the history's refs
// name: what the full pack must reach.
func compactTips(rec *Record, hist []HistoryEntry) []string {
	ids := refTips(rec.Refs)
	for _, e := range hist {
		ids = append(ids, refTips(e.Refs)...)
	}
	sort.Strings(ids)
	return compactStrings(ids)
}

func compactStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// borrow makes the objects the repo lacks readable from the archive: it
// indexes every pack of the chain into a temp bare repo and points g's
// alternates at it. Done only when an earlier snapshot names an object
// the repo no longer has, such as a deleted branch that was gc'd.
func (p *compactPlan) borrow(ctx context.Context, g *Git, get BlockGetter, tmp string) (func(), error) {
	dir, err := os.MkdirTemp(tmp, "git-ipfs-archive-compact-*")
	if err != nil {
		return nil, err
	}
	done := func() { os.RemoveAll(dir) }
	bare := &Git{Bin: g.Bin}
	if _, err := bare.run(ctx, nil, "init", "-q", "--bare", "--object-format="+g.Format.Name, dir); err != nil {
		done()
		return nil, err
	}
	ag := &Git{Bin: g.Bin, Dir: dir, Format: g.Format}
	indexed := map[string]bool{}
	for _, s := range p.chain {
		for _, pk := range s.Record.Packs {
			if indexed[pk.Pack.KeyString()] {
				continue
			}
			indexed[pk.Pack.KeyString()] = true
			if err := ag.indexPack(ctx, get, pk); err != nil {
				done()
				return nil, fmt.Errorf("reading snapshot %s's pack: %w", s.Root, err)
			}
		}
	}
	g.Alternates = quoteAlternate(filepath.Join(dir, "objects"))
	return func() { g.Alternates = ""; done() }, nil
}

// quoteAlternate quotes a path for GIT_ALTERNATE_OBJECT_DIRECTORIES, which
// splits on ':' (';' on Windows) unless the entry is C-quoted.
func quoteAlternate(p string) string {
	if !strings.ContainsAny(p, ":;\"\\") {
		return p
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(p) + `"`
}

// carriedLFS is every LFS object the chain lists, each once, with its file
// blocks put again so the compacted snapshot holds them.
func (p *compactPlan) carriedLFS(get BlockGetter, put BlockSink) (map[string]LFSObject, error) {
	out := map[string]LFSObject{}
	copied := map[string]bool{}
	for _, s := range p.chain {
		objs, err := readLFS(get, s.Record)
		if err != nil {
			return nil, fmt.Errorf("LFS list of %s: %w", s.Root, err)
		}
		for _, o := range objs {
			if _, ok := out[o.OID]; ok {
				continue
			}
			if err := copyFile(get, o.File.Cid, put, copied, 0); err != nil {
				return nil, fmt.Errorf("LFS object %s: %w", o.OID, err)
			}
			out[o.OID] = o
		}
	}
	return out, nil
}

// copyFile puts every block of a stored UnixFS file again, each once:
// copied keeps a DAG that names one block many times from costing more.
func copyFile(get BlockGetter, c cid.Cid, put BlockSink, copied map[string]bool, depth int) error {
	if depth > maxFileDepth {
		return errors.New("file DAG is too deep")
	}
	if copied[c.KeyString()] {
		return nil
	}
	copied[c.KeyString()] = true
	data, err := get(c)
	if err != nil {
		return err
	}
	switch c.Type() {
	case cid.Raw:
		return put(c, data)
	case cid.DagProtobuf:
	default:
		return fmt.Errorf("block %s: not part of a UnixFS file", c)
	}
	b, err := blocks.NewBlockWithCid(data, c)
	if err != nil {
		return err
	}
	pn, err := merkledag.DecodeProtobufBlock(b)
	if err != nil {
		return err
	}
	for _, l := range pn.Links() {
		if err := copyFile(get, l.Cid, put, copied, depth+1); err != nil {
			return err
		}
	}
	return put(c, data)
}
