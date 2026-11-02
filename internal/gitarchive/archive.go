package gitarchive

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/ipfs/go-cid"
)

// Options steer Archive.
type Options struct {
	Git string // git binary; "git" when empty

	// Prev is the snapshot to build on and PrevRoot its record's CID. With
	// them, Archive writes a delta: a thin pack of objects Prev's refs do
	// not reach, and a record whose Prev points back.
	Prev     *Record
	PrevRoot cid.Cid

	// Instructions are copied into the record for whatever stores it. Nil
	// keeps Prev's; an empty map clears them.
	Instructions map[string]string

	TempDir string // for the pack while it is indexed; os.TempDir() when empty
	Tool    string // name and version of the writing tool, recorded

	// LogChunk is how many commits go in one log block; 2000 when zero.
	LogChunk int
}

// Result is what Archive made.
type Result struct {
	Root      cid.Cid
	Record    *Record
	Unchanged bool  // refs match Prev: Root is PrevRoot and nothing was written
	Objects   int   // objects in the new pack
	PackBytes int64 // its size
}

// Archive reads the git repo at path and sends a snapshot's blocks to put:
// one pack (thin when building on opts.Prev), its .idx, the commit log, and
// the record, which is put last and is the root.
func Archive(ctx context.Context, path string, o Options, put BlockSink) (*Result, error) {
	g, err := OpenRepo(ctx, o.Git, path)
	if err != nil {
		return nil, err
	}
	if o.Prev != nil {
		if !o.PrevRoot.Defined() {
			return nil, errors.New("a previous record needs its CID")
		}
		if o.Prev.ObjectFormat != g.Format.Name {
			return nil, fmt.Errorf("previous snapshot is a %s repo, %s is %s", o.Prev.ObjectFormat, path, g.Format.Name)
		}
	}
	rec := &Record{Kind: Kind, Version: RecordVersion, ObjectFormat: g.Format.Name, Instructions: o.Instructions, Tool: o.Tool}
	if o.Instructions == nil && o.Prev != nil {
		rec.Instructions = o.Prev.Instructions // carried forward unless replaced
	}
	if len(rec.Instructions) == 0 {
		rec.Instructions = nil
	}
	if rec.Refs, err = g.Refs(ctx); err != nil {
		return nil, err
	}
	if rec.Head, err = g.Head(ctx); err != nil {
		return nil, err
	}
	if rec.Remotes, err = g.Remotes(ctx); err != nil {
		return nil, err
	}
	if rec.Shallow, err = g.Shallow(); err != nil {
		return nil, err
	}
	if err := checkText(rec); err != nil {
		return nil, err
	}
	if o.Prev != nil && sameState(o.Prev, rec) {
		return &Result{Root: o.PrevRoot, Record: o.Prev, Unchanged: true}, nil
	}
	if o.Prev != nil {
		rec.Prev = &Link{o.PrevRoot}
	}

	tips := refTips(rec.Refs)
	var exclude []string
	if o.Prev != nil {
		// Only exclude tips the repo still has; a gc'd one would make
		// pack-objects fail. Leaving it out just re-stores a few objects.
		if exclude, err = g.Present(ctx, refTips(o.Prev.Refs)); err != nil {
			return nil, err
		}
	}
	res := &Result{Record: rec}
	if len(tips) > 0 {
		pk, commits, err := writePack(ctx, g, tips, exclude, o.Prev != nil, o.TempDir, put)
		if err != nil {
			return nil, err
		}
		if pk != nil {
			rec.Packs = append(rec.Packs, *pk)
			res.Objects, res.PackBytes = pk.Objects, pk.Size
		}
		if rec.Log, err = writeLog(commits, o.LogChunk, put); err != nil {
			return nil, err
		}
		rec.Commits = len(commits)
	}
	if err := rec.check(); err != nil {
		return nil, err
	}
	c, b, err := encodeBlock(rec)
	if err != nil {
		return nil, err
	}
	if err := put(c, b); err != nil {
		return nil, err
	}
	res.Root = c
	return res, nil
}

// refTips is every object a ref names, deduplicated and sorted.
func refTips(refs []Ref) []string {
	var ids []string
	for _, r := range refs {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return slices.Compact(ids)
}

func sameState(a, b *Record) bool {
	if a.Head != b.Head || !slices.Equal(a.Shallow, b.Shallow) || !slices.Equal(a.Refs, b.Refs) || !slices.Equal(a.Remotes, b.Remotes) || len(a.Instructions) != len(b.Instructions) {
		return false
	}
	for k, v := range a.Instructions {
		if b.Instructions[k] != v {
			return false
		}
	}
	return true
}

// checkText refuses names DAG-CBOR cannot hold as text. git allows any
// bytes in a ref name; storing a lossy copy would restore the wrong ref.
func checkText(r *Record) error {
	for _, ref := range r.Refs {
		if !utf8.ValidString(ref.Name) {
			return fmt.Errorf("ref name %q is not UTF-8; rename it to archive this repo", ref.Name)
		}
	}
	if !utf8.ValidString(r.Head) {
		return fmt.Errorf("HEAD %q is not UTF-8", r.Head)
	}
	for i := range r.Remotes {
		r.Remotes[i].Name = text([]byte(r.Remotes[i].Name))
		r.Remotes[i].URL = text([]byte(r.Remotes[i].URL))
	}
	return nil
}

// writePack runs pack-objects into a temp file, indexes it (SHA-1DC on every
// object), and stores the pack and its .idx as UnixFS files. It returns nil
// when there are no new objects.
func writePack(ctx context.Context, g *Git, tips, exclude []string, thin bool, tmp string, put BlockSink) (*Pack, []Commit, error) {
	f, err := os.CreateTemp(tmp, "git-ipfs-archive-*.pack")
	if err != nil {
		return nil, nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := g.PackObjects(ctx, f, tips, exclude, thin); err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	cf, err := g.catFile(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer cf.close()
	var commits []Commit
	ix, err := IndexPack(f, st.Size(), g.Format, cf.get, func(o PackObject, data []byte) error {
		if o.Type == objCommit {
			commits = append(commits, parseCommit(o.ID, data))
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, nil, fmt.Errorf("indexing the pack git made: %w", err)
	}
	if len(ix.Objects) == 0 {
		return nil, commits, nil
	}
	var idx bytes.Buffer
	if err := WriteIdx(&idx, ix); err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, nil, err
	}
	pc, err := putFile(f, put)
	if err != nil {
		return nil, nil, err
	}
	ic, err := putFile(&idx, put)
	if err != nil {
		return nil, nil, err
	}
	return &Pack{Pack: Link{pc}, Idx: Link{ic}, Objects: len(ix.Objects), Size: st.Size(), Thin: ix.Thin}, commits, nil
}

// writeLog stores commits newest first, in chunks.
func writeLog(commits []Commit, chunk int, put BlockSink) ([]Link, error) {
	if chunk <= 0 {
		chunk = 2000
	}
	sort.Slice(commits, func(i, j int) bool {
		a, b := commits[i].Committer.Time, commits[j].Committer.Time
		if a != b {
			return a > b
		}
		return commits[i].ID < commits[j].ID
	})
	var links []Link
	for i := 0; i < len(commits); i += chunk {
		c, b, err := encodeBlock(LogChunk{Commits: commits[i:min(i+chunk, len(commits))]})
		if err != nil {
			return nil, err
		}
		if err := put(c, b); err != nil {
			return nil, err
		}
		links = append(links, Link{c})
	}
	return links, nil
}

// HexID is a convenience for tests and tools.
func HexID(b []byte) string { return hex.EncodeToString(b) }
