package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto/atprototest"
	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/repo"
	"github.com/jbenet/ipfs2fil/internal/repo/repotest"
)

func export(t *testing.T, archiveDir string, opts ExportOptions) (string, *ExportResult) {
	t.Helper()
	dir := t.TempDir()
	res, err := Export(context.Background(), archiveDir, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

// blockSet lists a repo's blocks by multihash.
func blockSet(t *testing.T, r repo.Repo) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := r.ForEachBlock(context.Background(), func(h multihash.Multihash, _ int) error {
		out[string(h)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameRepo checks that an export of the default kind gives back the repo's
// blocks, pins and MFS root.
func sameRepo(t *testing.T, r repo.Repo, dir string, res *ExportResult) {
	t.Helper()
	ctx := context.Background()
	got, err := repo.Open(filepath.Join(dir, ExportBlocks))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	want, have := blockSet(t, r), blockSet(t, got)
	for h := range want {
		if !have[h] {
			t.Errorf("block %s is not in the export", multihash.Multihash(h).B58String())
		}
	}
	for h := range have {
		if !want[h] {
			t.Errorf("block %s is in the export but not the repo", multihash.Multihash(h).B58String())
		}
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wantPins, gotPins []string
	for _, p := range pins {
		wantPins = append(wantPins, p.Root.String()+" "+p.Mode.String()+" "+p.Name)
	}
	var listed []ExportPin
	b, err := os.ReadFile(filepath.Join(dir, ExportPins))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &listed); err != nil {
		t.Fatal(err)
	}
	for _, p := range listed {
		gotPins = append(gotPins, p.CID+" "+p.Mode+" "+p.Name)
	}
	sort.Strings(wantPins)
	sort.Strings(gotPins)
	if strings.Join(wantPins, "\n") != strings.Join(gotPins, "\n") {
		t.Errorf("pins:\n%s\nwant:\n%s", strings.Join(gotPins, "\n"), strings.Join(wantPins, "\n"))
	}
	mfs, ok, err := r.MFSRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[bool]string{true: mfs.String()}[ok]; res.MFS != want {
		t.Errorf("MFS root %q, want %q", res.MFS, want)
	}
	if len(res.Missing) != 0 {
		t.Errorf("missing %v", res.Missing)
	}
	if _, err := os.Stat(filepath.Join(dir, ExportImport)); err != nil {
		t.Error(err)
	}
}

// Every fs-repo fixture, 0 to 18, comes back with the same blocks, pins and
// MFS root, read from its pieces alone; and so does a running node's repo.
func TestExportFSRepos(t *testing.T) {
	names, _ := filepath.Glob("../../testdata/fixtures/*.tar.gz")
	if len(names) == 0 {
		t.Fatal("no fixtures in testdata/fixtures")
	}
	for _, f := range names {
		t.Run(strings.TrimSuffix(filepath.Base(f), ".tar.gz"), func(t *testing.T) {
			t.Parallel()
			r, err := repo.Open(filepath.Join(untar(t, f), "repo"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			adir := t.TempDir()
			build(t, r, adir)
			dir, res := export(t, adir, ExportOptions{})
			if res.Kind != repo.KindFSRepo {
				t.Errorf("kind %q", res.Kind)
			}
			sameRepo(t, r, dir, res)

			// The same repo through a node's RPC API. Kubo nodes have had
			// MFS since repo 3; the test server only serves repos that do.
			if _, ok, _ := r.MFSRoot(context.Background()); !ok {
				return
			}
			srv := httptest.NewServer(repotest.KuboRPC(r, false))
			defer srv.Close()
			rr, err := repo.Open(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer rr.Close()
			adir = t.TempDir()
			build(t, rr, adir)
			dir, res = export(t, adir, ExportOptions{})
			if res.Kind != repo.KindRPC {
				t.Errorf("kind %q", res.Kind)
			}
			sameRepo(t, rr, dir, res)
		})
	}
}

// writeCAR writes a CARv1 holding the sample repo's blocks under its pins,
// with one block twice, as some writers do.
func writeCAR(t *testing.T, path string, f *fakeRepo, roots ...cid.Cid) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	w, err := car.NewWriter(out, roots...)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range f.blocks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var first cid.Cid
	for _, k := range keys {
		c := guess(multihash.Multihash(k), f.blocks[k])
		if !first.Defined() {
			first = c
		}
		if err := w.Put(c, f.blocks[k]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Put(first, f.blocks[string(first.Hash())]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
}

// CAR repos come back as the very files: a CARv1, a CARv2 with its index,
// and a folder of them.
func TestExportCARFiles(t *testing.T) {
	f := sample(t, false)
	var roots []cid.Cid
	for _, p := range f.pins {
		roots = append(roots, p.Root)
	}
	src := t.TempDir()
	writeCAR(t, filepath.Join(src, "one.car"), f, roots[0])
	writeCAR(t, filepath.Join(src, "sub", "v1.car"), f, roots...)
	if err := carv2.WrapV1File(filepath.Join(src, "sub", "v1.car"), filepath.Join(src, "sub", "v2.car")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(src, "one.car"), filepath.Join(src, "sub", "v2.car"), src} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			r, err := repo.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			adir := t.TempDir()
			build(t, r, adir)
			dir, res := export(t, adir, ExportOptions{})
			base := path
			if fi, _ := os.Stat(path); !fi.IsDir() {
				base = filepath.Dir(path)
			}
			files := 0
			for _, ef := range res.Files {
				if ef.Same == nil || !*ef.Same {
					t.Errorf("%s: not the same as the original", ef.Path)
				}
				want, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(ef.Path)))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ef.Path)))
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s differs from the original (%v)", ef.Path, err)
				}
				files++
			}
			if wantFiles := map[bool]int{true: 3}[path == src]; files != max(wantFiles, 1) {
				t.Errorf("%d files exported", files)
			}
		})
	}
}

// A Bluesky account comes back as the directory ipfs2fil fetched: repo.car
// byte for byte, every blob, and the identity documents.
func TestExportATProto(t *testing.T) {
	atprototest.New(t)
	repo.ATProtoDir = t.TempDir()
	r, err := repo.Open("at://alice.test")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	adir := t.TempDir()
	build(t, r, adir)
	dir, res := export(t, adir, ExportOptions{})
	if res.Kind != repo.KindATProto {
		t.Errorf("kind %q", res.Kind)
	}
	fetched, _ := filepath.Glob(filepath.Join(repo.ATProtoDir, "*"))
	if len(fetched) != 1 {
		t.Fatalf("fetched %v", fetched)
	}
	n := 0
	err = filepath.WalkDir(fetched[0], func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(fetched[0], p)
		want, _ := os.ReadFile(p)
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs from the fetched copy (%v)\n got %q\nwant %q", rel, err, trim(got), trim(want))
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 5 {
		t.Errorf("only %d files fetched", n)
	}
	for _, ef := range res.Files {
		if ef.Path == "repo.car" && (ef.Same == nil || !*ef.Same) {
			t.Error("repo.car is not the same as fetched")
		}
	}
}

func trim(b []byte) []byte {
	if len(b) > 200 {
		return b[:200]
	}
	return b
}

// Export reads the pieces it lacks through Fetch.
func TestExportFetches(t *testing.T) {
	f := sample(t, false)
	adir := t.TempDir()
	res := build(t, f, adir)
	store := t.TempDir()
	for _, p := range res.Pieces {
		if err := os.Rename(p.Path, filepath.Join(store, filepath.Base(p.Path))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Export(context.Background(), adir, t.TempDir(), ExportOptions{}); err == nil {
		t.Fatal("exported with no pieces on disk")
	}
	fetched := 0
	dir, got := export(t, adir, ExportOptions{Fetch: func(_ context.Context, pc string) (io.ReadCloser, error) {
		fetched++
		return os.Open(filepath.Join(store, pc+".car"))
	}})
	if fetched == 0 {
		t.Error("nothing fetched")
	}
	sameRepo(t, f, dir, got)
	if left, _ := filepath.Glob(filepath.Join(dir, ".pieces-*")); len(left) != 0 {
		t.Errorf("fetched pieces left behind: %v", left)
	}
}

// Blocks a repo shares with an earlier one come from that repo's archive.
func TestExportShared(t *testing.T) {
	a, adir := sample(t, false), t.TempDir()
	ares := build(t, a, adir)
	held := map[string]SharedBlock{}
	for _, p := range ares.Manifest.Pieces {
		if p.BlockList == "" {
			continue
		}
		err := ReadBlockList(filepath.Join(adir, "index", p.BlockList), func(h multihash.Multihash) error {
			held[string(h)] = SharedBlock{Archive: "a", PieceCID: p.PieceCID}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	b, bdir := sample(t, true), t.TempDir()
	opts := testOptions()
	opts.Shared = func(h multihash.Multihash) (SharedBlock, bool) { sb, ok := held[string(h)]; return sb, ok }
	bres, err := Build(context.Background(), b, bdir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if bres.Manifest.Shared == nil || bres.Manifest.Shared.Blocks == 0 {
		t.Fatal("nothing shared")
	}
	if _, err := Export(context.Background(), bdir, t.TempDir(), ExportOptions{}); err == nil {
		t.Fatal("exported without the earlier archive's pieces")
	}
	dir, res := export(t, bdir, ExportOptions{Dirs: []string{adir}})
	sameRepo(t, b, dir, res)
}
