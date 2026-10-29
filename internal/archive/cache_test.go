package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

// countingRepo counts block reads and can fail after a number of them.
type countingRepo struct {
	*fakeRepo
	gets   int
	failAt int // fail the read after this many, if > 0
}

var errKilled = errors.New("killed")

func (c *countingRepo) Get(ctx context.Context, h multihash.Multihash) ([]byte, error) {
	c.gets++
	if c.failAt > 0 && c.gets > c.failAt {
		return nil, errKilled
	}
	return c.fakeRepo.Get(ctx, h)
}

var _ repo.Repo = (*countingRepo)(nil)

func samePack(t *testing.T, a, b *Result) {
	t.Helper()
	x, _ := json.Marshal(a.Manifest)
	y, _ := json.Marshal(b.Manifest)
	if !bytes.Equal(x, y) {
		t.Fatalf("manifests differ:\n%s\n%s", x, y)
	}
	if len(a.Pieces) != len(b.Pieces) {
		t.Fatalf("%d vs %d pieces", len(a.Pieces), len(b.Pieces))
	}
	for i := range a.Pieces {
		p, _ := os.ReadFile(a.Pieces[i].Path)
		q, _ := os.ReadFile(b.Pieces[i].Path)
		if len(p) == 0 || !bytes.Equal(p, q) {
			t.Fatalf("piece %d differs", i)
		}
	}
}

func TestUnchangedRepackReadsNothing(t *testing.T) {
	dir := t.TempDir()
	first := build(t, sample(t, false), dir)
	if first.Reused {
		t.Fatal("the first pack says it reused one")
	}
	r := &countingRepo{fakeRepo: sample(t, false)}
	again := build(t, r, dir)
	if !again.Reused || r.gets != 0 {
		t.Fatalf("an unchanged re-pack read %d blocks (reused %v)", r.gets, again.Reused)
	}
	check(t, again)
	samePack(t, first, again)

	// Without the cache, the full pack finds the same data and keeps the
	// same archive.
	os.Remove(filepath.Join(dir, CacheFile))
	full := build(t, sample(t, false), dir)
	samePack(t, again, full)

	// A missing piece CAR means packing again.
	os.Remove(full.Pieces[2].Path)
	r = &countingRepo{fakeRepo: sample(t, false)}
	res := build(t, r, dir)
	if r.gets == 0 {
		t.Fatalf("a pack with a CAR gone read no blocks")
	}
	samePack(t, full, res)
}

func TestOptionsChangeRepacks(t *testing.T) {
	dir := t.TempDir()
	build(t, sample(t, false), dir)
	opts := testOptions()
	opts.Orphans = false
	res, err := Build(context.Background(), sample(t, false), dir, opts)
	if err != nil || res.Reused {
		t.Fatalf("a pack without orphans reused the one with them (%v)", err)
	}
	opts.Shared = func(multihash.Multihash) (SharedBlock, bool) { return SharedBlock{}, false }
	// A lookup that finds nothing gives the same data, so the same archive.
	if res, err = Build(context.Background(), sample(t, false), dir, opts); err != nil || !res.Reused {
		t.Fatalf("a lookup that finds nothing re-made the archive (%v)", err)
	}
	opts.Shared = func(multihash.Multihash) (SharedBlock, bool) { return SharedBlock{Archive: "x", PieceCID: "y"}, true }
	if res, err = Build(context.Background(), sample(t, false), dir, opts); err != nil || res.Reused {
		t.Fatalf("a lookup that now finds blocks reused the last pack (%v)", err)
	}
}

func TestEditRewritesOnlyChangedPieces(t *testing.T) {
	dir := t.TempDir()
	build(t, sample(t, false), dir)
	r := &countingRepo{fakeRepo: sample(t, true)}
	edited := build(t, r, dir)
	if edited.Reused {
		t.Fatal("an edited repo reused the last pack")
	}
	check(t, edited)
	fresh := build(t, sample(t, true), t.TempDir())
	// The history differs, so compare the data pieces.
	if len(dataPieces(edited)) != len(dataPieces(fresh)) {
		t.Fatalf("%d vs %d data pieces", len(dataPieces(edited)), len(dataPieces(fresh)))
	}
	for c := range dataPieces(fresh) {
		if !dataPieces(edited)[c] {
			t.Fatalf("data piece %s missing after an edit in place", c)
		}
	}
	// The walk reads each block once; only the changed pieces read theirs again.
	if extra := r.gets - len(r.blocks); extra <= 0 || extra > len(r.blocks)/2 {
		t.Fatalf("%d reads for %d blocks", r.gets, len(r.blocks))
	}
}

func TestStoppedPackKeepsWrittenPieces(t *testing.T) {
	dir := t.TempDir()
	whole := build(t, sample(t, false), t.TempDir())
	r := &countingRepo{fakeRepo: sample(t, false)}
	r.failAt = len(r.blocks) + len(r.blocks)/2 // part way through writing pieces
	if _, err := Build(context.Background(), r, dir, testOptions()); !errors.Is(err, errKilled) {
		t.Fatalf("got %v", err)
	}
	c := readCache(dir)
	if len(c.Pieces) == 0 || c.Inputs != "" {
		t.Fatalf("after a stopped pack the cache has %d pieces, inputs %q", len(c.Pieces), c.Inputs)
	}
	r2 := &countingRepo{fakeRepo: sample(t, false)}
	res := build(t, r2, dir)
	check(t, res)
	samePieces(t, res, whole)
	if extra := r2.gets - len(r2.blocks); extra >= len(r2.blocks)*3/4 {
		t.Fatalf("the re-run read %d blocks again of %d", extra, len(r2.blocks))
	}
}

func samePieces(t *testing.T, a, b *Result) {
	t.Helper()
	if a.Manifest.Root != b.Manifest.Root || len(a.Pieces) != len(b.Pieces) {
		t.Fatalf("root %s vs %s, %d vs %d pieces", a.Manifest.Root, b.Manifest.Root, len(a.Pieces), len(b.Pieces))
	}
	for i := range a.Pieces {
		p, _ := os.ReadFile(a.Pieces[i].Path)
		q, _ := os.ReadFile(b.Pieces[i].Path)
		if len(p) == 0 || !bytes.Equal(p, q) {
			t.Fatalf("piece %d differs", i)
		}
	}
}

// Without a cache (lost, or a pack from before it), pieces whose block
// lists match the last pack's keep their CARs: an edit reads no more blocks
// than it does with the cache.
func TestNoCacheMatchesBlockLists(t *testing.T) {
	withCache, noCache := t.TempDir(), t.TempDir()
	build(t, sample(t, false), withCache)
	build(t, sample(t, false), noCache)
	os.Remove(filepath.Join(noCache, CacheFile))

	a := &countingRepo{fakeRepo: sample(t, true)}
	x := build(t, a, withCache)
	b := &countingRepo{fakeRepo: sample(t, true)}
	y := build(t, b, noCache)
	if b.gets != a.gets {
		t.Errorf("without the cache the edit read %d blocks, with it %d", b.gets, a.gets)
	}
	samePack(t, x, y)

	// The matches are cached for next time.
	r := &countingRepo{fakeRepo: sample(t, true)}
	if res := build(t, r, noCache); !res.Reused || r.gets != 0 {
		t.Errorf("the next pack read %d blocks (reused %v)", r.gets, res.Reused)
	}
}

func TestNewVersionKeepsUnchangedArchive(t *testing.T) {
	dir := t.TempDir()
	first := build(t, sample(t, false), dir)
	opts := testOptions()
	opts.Tool = "ipfs2fil v9"
	for _, cached := range []bool{true, false} {
		if !cached {
			os.Remove(filepath.Join(dir, CacheFile))
		}
		res, err := Build(context.Background(), sample(t, false), dir, opts)
		if err != nil || !res.Reused {
			t.Fatalf("a new version re-made an unchanged archive (cache %v, %v)", cached, err)
		}
		samePack(t, first, res)
	}
	// A changed repo still packs again.
	if res, err := Build(context.Background(), sample(t, true), dir, opts); err != nil || res.Reused {
		t.Fatalf("an edited repo reused the last pack (%v)", err)
	}
}
