package archive

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// packed reads the archive in dir as a finished pack left it.
func packed(t *testing.T, dir string) *Result {
	t.Helper()
	m, err := manifest.Read(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	res := &Result{Manifest: m}
	for _, p := range m.Pieces {
		if p.PieceCID == "" {
			t.Fatalf("piece %d has no PieceCID", p.Index)
		}
		res.Pieces = append(res.Pieces, Piece{Piece: p, Path: filepath.Join(dir, "pieces", p.PieceCID+".car")})
	}
	return res
}

// packAll builds each repo into its dir with one bundle, as pack --all does.
func packAll(t *testing.T, opts Options, repos []repo.Repo, dirs []string, records bool) ([]*Result, []Piece) {
	t.Helper()
	opts.Bundle = NewBundle(opts.MaxPiece)
	var out []*Result
	for i, r := range repos {
		o := opts
		if records {
			o.Record = []byte("record of repo " + string(rune('a'+i)))
		}
		res, err := Build(context.Background(), r, dirs[i], o)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
	}
	bundles, err := opts.Bundle.Finish()
	if err != nil {
		t.Fatal(err)
	}
	for i := range out {
		reused := out[i].Reused
		out[i] = packed(t, dirs[i])
		out[i].Reused = reused
	}
	return out, bundles
}

func bundleOf(t *testing.T, res *Result) manifest.Piece {
	t.Helper()
	var got []manifest.Piece
	for _, p := range res.Manifest.Pieces {
		if p.Part == manifest.PartBundle {
			got = append(got, p)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d bundle pieces, want 1", len(got))
	}
	return got[0]
}

// One repo packed with a bundle: its small data piece, index, explorer and
// head go in one piece, and it exports exactly.
func TestBundleOneRepo(t *testing.T) {
	dir := t.TempDir()
	r := sample(t, false)
	res, bundles := packAll(t, onePiece(), []repo.Repo{r}, []string{dir}, false)
	check(t, res[0])
	if len(bundles) != 1 || len(res[0].Manifest.Pieces) != 1 {
		t.Fatalf("%d bundles, %d pieces; want 1 and 1", len(bundles), len(res[0].Manifest.Pieces))
	}
	p := res[0].Manifest.Pieces[0]
	if p.Part != manifest.PartBundle || p.BlockList == "" || p.DataBytes == 0 || p.DataBytes >= p.CARBytes {
		t.Errorf("piece %+v", p)
	}
	out, got := export(t, dir, ExportOptions{})
	sameRepo(t, r, out, got)

	// Nothing changed: nothing is bundled, and the archive is the same.
	again, bundles := packAll(t, onePiece(), []repo.Repo{sample(t, false)}, []string{dir}, false)
	if len(bundles) != 0 || !again[0].Reused || again[0].Manifest.Root != res[0].Manifest.Root {
		t.Errorf("re-run: %d bundles, reused %v", len(bundles), again[0].Reused)
	}
}

// Several repos packed in one go share one bundle piece, whose header names
// each archive's root and record, records first; each exports exactly.
// After a push, an edit to one repo makes one small bundle with only its
// changes, keeping the stored one for the rest of its data.
func TestBundleManyRepos(t *testing.T) {
	repos := []repo.Repo{sample(t, false), manyFiles(t, false), bigFile(t, 7)}
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	opts := onePiece()
	opts.MaxPiece = 32 << 20
	res, bundles := packAll(t, opts, repos, dirs, true)
	if len(bundles) != 1 {
		t.Fatalf("%d bundles, want 1", len(bundles))
	}
	b := bundles[0]
	f, err := os.Open(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := car.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Roots) != 2*len(repos) {
		t.Fatalf("bundle header names %d roots, want %d", len(cr.Roots), 2*len(repos))
	}
	for i := range repos {
		if cr.Roots[2*i].String() != res[i].Manifest.Root {
			t.Errorf("header root %d is %s, want archive %d's root", 2*i, cr.Roots[2*i], i)
		}
		c, data, err := cr.Next()
		if err != nil || !c.Equals(cr.Roots[2*i+1]) || string(data) != "record of repo "+string(rune('a'+i)) {
			t.Errorf("block %d is %s, not archive %d's record", i, c, i)
		}
	}
	f.Close()
	for i, r := range repos {
		check(t, res[i])
		if p := bundleOf(t, res[i]); p.PieceCID != b.PieceCID {
			t.Errorf("repo %d: bundle %s, want %s", i, p.PieceCID, b.PieceCID)
		}
		out, got := export(t, dirs[i], ExportOptions{})
		sameRepo(t, r, out, got)
	}

	for _, d := range dirs {
		stored(t, d)
	}
	edited := []repo.Repo{sample(t, true), manyFiles(t, false), bigFile(t, 7)}
	res2, bundles := packAll(t, opts, edited, dirs, true)
	if len(bundles) != 1 {
		t.Fatalf("after an edit: %d bundles, want 1", len(bundles))
	}
	if !res2[1].Reused || !res2[2].Reused || res2[0].Reused {
		t.Errorf("reused: %v %v %v, want only the unchanged repos", res2[0].Reused, res2[1].Reused, res2[2].Reused)
	}
	if bundles[0].CARBytes*4 > b.CARBytes {
		t.Errorf("the edit's bundle is %d bytes, the first %d", bundles[0].CARBytes, b.CARBytes)
	}
	kept := false
	for _, p := range res2[0].Manifest.Pieces {
		if p.PieceCID == b.PieceCID {
			kept = true
		}
	}
	if !kept {
		t.Error("the edited repo does not keep the stored bundle for its unchanged blocks")
	}
	check(t, res2[0])
	for i, r := range edited {
		out, got := export(t, dirs[i], ExportOptions{})
		sameRepo(t, r, out, got)
	}

	// Pushed again, nothing changes and nothing is bundled.
	for _, d := range dirs {
		stored(t, d)
	}
	res3, bundles := packAll(t, opts, []repo.Repo{sample(t, true), manyFiles(t, false), bigFile(t, 7)}, dirs, true)
	if len(bundles) != 0 {
		t.Errorf("an unchanged re-run made %d bundles", len(bundles))
	}
	for i := range res3 {
		if res3[i].Manifest.Root != res2[i].Manifest.Root {
			t.Errorf("repo %d: a re-run made a new archive", i)
		}
	}
}

// bigFile is a repo of one pinned file.
func bigFile(t *testing.T, seed int64) *fakeRepo {
	f := newFake()
	n := f.file(t, random(seed, 200000), true)
	f.pins = []repo.Pin{{Root: n.Cid(), Mode: repo.Recursive, Name: "big"}}
	return f
}

// A block two repos both have goes in their bundle once.
func TestBundleStoresSharedBlockOnce(t *testing.T) {
	a, b := newFake(), newFake()
	same := random(9, 50000)
	fa := a.file(t, same, true)
	fb := b.file(t, same, true)
	other := b.file(t, random(10, 3000), true)
	a.pins = []repo.Pin{{Root: fa.Cid(), Mode: repo.Recursive}}
	b.pins = []repo.Pin{{Root: fb.Cid(), Mode: repo.Recursive}, {Root: other.Cid(), Mode: repo.Recursive}}
	dirs := []string{t.TempDir(), t.TempDir()}
	_, bundles := packAll(t, onePiece(), []repo.Repo{a, b}, dirs, true)
	if len(bundles) != 1 {
		t.Fatalf("%d bundles", len(bundles))
	}
	f, err := os.Open(bundles[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cr, err := car.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for {
		c, _, err := cr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen[c.KeyString()] {
			t.Errorf("block %s is in the bundle twice", c)
		}
		seen[c.KeyString()] = true
	}
	if !seen[fa.Cid().KeyString()] {
		t.Error("the shared file is not in the bundle")
	}
	for i, r := range []repo.Repo{a, b} {
		out, got := export(t, dirs[i], ExportOptions{})
		sameRepo(t, r, out, got)
	}
}

// Repos whose changes do not fit in one piece get a bundle each.
func TestBundleRollsOver(t *testing.T) {
	repos := []repo.Repo{bigFile(t, 1), bigFile(t, 2), bigFile(t, 3)}
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	opts := onePiece()
	opts.MinPiece, opts.MaxPiece = 400<<10, 1<<20
	res, bundles := packAll(t, opts, repos, dirs, true)
	if len(bundles) < 2 {
		t.Fatalf("%d bundles, want more than one", len(bundles))
	}
	for i, r := range repos {
		check(t, res[i])
		if p := bundleOf(t, res[i]); p.CARBytes > opts.MaxPiece {
			t.Errorf("bundle of %d bytes", p.CARBytes)
		}
		out, got := export(t, dirs[i], ExportOptions{})
		sameRepo(t, r, out, got)
	}
}

// A pack whose bundle is never written leaves the archive incomplete, and
// the next pack makes it again.
func TestUnfinishedBundleRepacks(t *testing.T) {
	dir := t.TempDir()
	opts := onePiece()
	opts.Bundle = NewBundle(opts.MaxPiece)
	if _, err := Build(context.Background(), sample(t, false), dir, opts); err != nil {
		t.Fatal(err)
	}
	opts.Bundle.Discard()
	res, bundles := packAll(t, onePiece(), []repo.Repo{sample(t, false)}, []string{dir}, false)
	if res[0].Reused || len(bundles) != 1 {
		t.Fatalf("reused %v, %d bundles", res[0].Reused, len(bundles))
	}
	check(t, res[0])
	ents, _ := os.ReadDir(filepath.Join(dir, "pieces"))
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".car" {
			t.Errorf("left over: %s", e.Name())
		}
	}
}

// With big data pieces, only the last, small one goes in the bundle; the
// meta pieces already stored stay out of it.
func TestBundleBigRepo(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions()
	r := sample(t, false)
	res, _ := packAll(t, opts, []repo.Repo{r}, []string{dir}, true)
	check(t, res[0])
	data, inBundle := 0, 0
	for _, p := range res[0].Manifest.Pieces {
		if p.BlockList != "" {
			data++
			if p.Part == manifest.PartBundle {
				inBundle++
			}
		}
	}
	if data < 3 || inBundle > 1 {
		t.Errorf("%d data pieces, %d of them the bundle", data, inBundle)
	}
	out, got := export(t, dir, ExportOptions{})
	sameRepo(t, r, out, got)
	stored(t, dir)

	// An edit: the stored meta pieces the edit leaves alone are kept.
	r2 := manyFiles(t, false)
	d2 := t.TempDir()
	packAll(t, opts, []repo.Repo{r2}, []string{d2}, true)
	before := stored(t, d2)
	res2, _ := packAll(t, opts, []repo.Repo{manyFiles(t, true)}, []string{d2}, true)
	had := map[string]bool{}
	for _, p := range before.Pieces {
		had[p.PieceCID] = true
	}
	keptMeta := 0
	for _, p := range res2[0].Manifest.Pieces {
		if (p.Part == manifest.PartMeta || p.Part == manifest.PartIndex) && had[p.PieceCID] {
			keptMeta++
		}
	}
	if keptMeta == 0 {
		t.Error("no stored meta piece is kept")
	}
	check(t, res2[0])
}
