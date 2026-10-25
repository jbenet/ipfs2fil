package archive

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

// TestFixtures packs every real fs-repo fixture (0 to 18) and checks the
// pieces hold every block reachable from the archive root, twice, byte for byte.
func TestFixtures(t *testing.T) {
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
			a := build(t, r, t.TempDir())
			check(t, a)
			if len(a.Missing) != 0 {
				t.Errorf("missing blocks %v", a.Missing)
			}
			if len(a.Manifest.Roots) == 0 {
				t.Error("no roots")
			}
			b := build(t, r, t.TempDir())
			if b.Manifest.Root != a.Manifest.Root || len(b.Pieces) != len(a.Pieces) {
				t.Fatalf("second pack differs: %s vs %s", b.Manifest.Root, a.Manifest.Root)
			}
			for i := range a.Pieces {
				if a.Pieces[i].PieceCID != b.Pieces[i].PieceCID {
					t.Fatalf("piece %d differs between packs", i)
				}
			}
		})
	}
}

func untar(t *testing.T, archive string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return dir
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("bad path %q in %s", h.Name, archive)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			var w *os.File
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				if w, err = os.Create(p); err == nil {
					_, err = io.Copy(w, tr)
					w.Close()
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
