package archive

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	format "github.com/ipfs/go-ipld-format"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// onePiece fits the sample repo in one data piece, as a small repo is with
// the default 512 MiB minimum.
func onePiece() Options {
	o := testOptions()
	o.MinPiece, o.MaxPiece = 4<<20, 8<<20
	return o
}

// stored records every piece of the archive in dir as held by two copies,
// as a push does.
func stored(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	path := filepath.Join(dir, ManifestFile)
	m, err := manifest.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Copies = nil
	for c := range 2 {
		ids := map[int]string{}
		for _, p := range m.Pieces {
			ids[p.Index] = fmt.Sprint(100*c + p.Index)
		}
		m.Copies = append(m.Copies, manifest.Copy{Provider: fmt.Sprint(c), DataSetID: fmt.Sprint(c), PieceIDs: ids})
	}
	if err := m.Write(path); err != nil {
		t.Fatal(err)
	}
	return m
}

// After a push, an edit to a small repo keeps its stored data piece and
// adds one small piece with the new blocks, instead of a new big piece.
func TestEditKeepsStoredPiece(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := Build(ctx, sample(t, false), dir, onePiece())
	if err != nil {
		t.Fatal(err)
	}
	var old manifest.Piece
	for _, p := range first.Manifest.Pieces {
		if p.BlockList != "" {
			old = p
		}
	}
	if n := len(dataPieces(first)); n != 1 {
		t.Fatalf("%d data pieces, want 1", n)
	}
	stored(t, dir)

	edited := sample(t, true)
	res, err := Build(ctx, edited, dir, onePiece())
	if err != nil {
		t.Fatal(err)
	}
	check(t, res)
	var fresh int64
	kept := false
	for _, p := range res.Manifest.Pieces {
		switch {
		case p.PieceCID == old.PieceCID:
			kept = true
			if p.Snapshot != 0 {
				t.Errorf("the kept piece says snapshot %d", p.Snapshot)
			}
		case p.BlockList != "":
			fresh += p.CARBytes
		}
	}
	if !kept {
		t.Fatal("the stored piece is not kept")
	}
	if fresh == 0 || fresh*10 > old.CARBytes {
		t.Errorf("the edit added %d bytes of data pieces next to a %d byte piece", fresh, old.CARBytes)
	}
	// The kept piece keeps its piece ids; the new one has none yet.
	for _, c := range res.Manifest.Copies {
		for _, p := range res.Manifest.Pieces {
			if (c.PieceIDs[p.Index] != "") != (p.PieceCID == old.PieceCID) {
				t.Errorf("copy %s: piece %d (%s) id %q", c.Provider, p.Index, p.PieceCID, c.PieceIDs[p.Index])
			}
		}
	}
	dirOut, got := export(t, dir, ExportOptions{})
	sameRepo(t, edited, dirOut, got)

	// Pushed, the same repo again gives the same archive.
	stored(t, dir)
	again, err := Build(ctx, sample(t, true), dir, onePiece())
	if err != nil {
		t.Fatal(err)
	}
	if again.Manifest.Root != res.Manifest.Root {
		t.Errorf("a re-run made a new archive: %s, was %s", again.Manifest.Root, res.Manifest.Root)
	}
}

// A stored piece the repo has mostly moved away from is not kept: the few
// blocks still in the repo go in a new piece with the rest.
func TestMostlyDeadPieceIsNotKept(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := Build(ctx, sample(t, false), dir, onePiece())
	if err != nil {
		t.Fatal(err)
	}
	stored(t, dir)
	// The sample's "site" pin, made the same way, and a big new file.
	f := newFake()
	site := f.dir(t, map[string]format.Node{"x": f.file(t, random(101, 3000), true)})
	big := f.file(t, random(500, 300000), false)
	f.pins = []repo.Pin{{Root: site.Cid(), Mode: repo.Recursive, Name: "site"}, {Root: big.Cid(), Mode: repo.Recursive}}
	res, err := Build(ctx, f, dir, onePiece())
	if err != nil {
		t.Fatal(err)
	}
	check(t, res)
	for c := range dataPieces(first) {
		if dataPieces(res)[c] {
			t.Errorf("piece %s is kept with under %.0f%% of it live", c, 100*KeepLive)
		}
	}
	dirOut, got := export(t, dir, ExportOptions{})
	sameRepo(t, f, dirOut, got)
}
