package gitarchive

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
)

// put encodes v as a block in bs.
func putRecord(t *testing.T, bs *Blocks, v any) cid.Cid {
	t.Helper()
	c, b, err := encodeBlock(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.Put(c, b); err != nil {
		t.Fatal(err)
	}
	return c
}

// A record that names one log chunk many times is read once per chunk, not
// once per mention: a small CAR must not make the explorer read without end.
func TestSummaryRepeatedLogChunk(t *testing.T) {
	needGit(t)
	bs := NewBlocks()
	res := archive(t, newRepo(t, ""), nil, bs)
	rec := *res.Record
	for range 100000 {
		rec.Log = append(rec.Log, res.Record.Log[0])
	}
	root := putRecord(t, bs, rec)
	gets := 0
	s, err := Summarize(func(c cid.Cid) ([]byte, error) { gets++; return bs.Get(c) }, root, "x")
	if err != nil {
		t.Fatal(err)
	}
	if gets > 10 || len(s.Commits) > 10 {
		t.Fatalf("%d block reads, %d commits listed", gets, len(s.Commits))
	}
}

// A record that lists one pack many times indexes it once.
func TestRestoreRepeatedPack(t *testing.T) {
	needGit(t)
	bs := NewBlocks()
	res := archive(t, newRepo(t, ""), nil, bs)
	rec := *res.Record
	for range 50 {
		rec.Packs = append(rec.Packs, res.Record.Packs[0])
	}
	root := putRecord(t, bs, rec)
	packs := 0
	_, err := Restore(t.Context(), bs.Get, root, filepath.Join(t.TempDir(), "r.git"), RestoreOptions{Log: func(f string, a ...any) {
		if strings.HasPrefix(f, "pack") {
			packs++
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if packs != 1 {
		t.Fatalf("indexed %d packs", packs)
	}
}

// Submodule records that fan out (the same record named under many names,
// level after level) cannot make restore --recurse do exponential work.
func TestRestoreSubmoduleFanout(t *testing.T) {
	needGit(t)
	bs := NewBlocks()
	res := archive(t, newRepo(t, ""), nil, bs)
	leaf := *res.Record
	head := leaf.Refs[0].ID
	c := putRecord(t, bs, leaf)
	for level := range 6 {
		r := leaf
		r.Submodules = nil
		for i := range 4 {
			r.Submodules = append(r.Submodules, Submodule{Name: fmt.Sprintf("l%d-%d", level, i), Path: fmt.Sprint(i), Commit: head, Record: &Link{c}})
		}
		c = putRecord(t, bs, r)
	}
	subs := 0
	_, err := Restore(t.Context(), bs.Get, c, filepath.Join(t.TempDir(), "r.git"), RestoreOptions{Recurse: true, Log: func(f string, a ...any) {
		if strings.HasPrefix(f, "submodule") {
			subs++
		}
	}})
	if subs > maxSubmoduleRestores {
		t.Fatalf("restored %d submodules (4^6 = 4096 without a bound): %v", subs, err)
	}
	if err == nil || !strings.Contains(err.Error(), "submodule") {
		t.Fatalf("no error past the bound: %v", err)
	}
}
