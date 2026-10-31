package repo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
)

func rawBlock(t *testing.T, s string) (cid.Cid, []byte) {
	t.Helper()
	h, err := multihash.Sum([]byte(s), multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.Raw, h), []byte(s)
}

// writeCAR writes a CARv1 naming the first block as its root.
func writeCAR(t *testing.T, path string, datas ...string) []cid.Cid {
	t.Helper()
	var cs []cid.Cid
	var bs [][]byte
	for _, d := range datas {
		c, b := rawBlock(t, d)
		cs = append(cs, c)
		bs = append(bs, b)
	}
	var buf bytes.Buffer
	w, err := car.NewWriter(&buf, cs[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range cs {
		if err := w.Put(cs[i], bs[i]); err != nil {
			t.Fatal(err)
		}
	}
	w.Finish() // a small CAR gets no PieceCID, which is fine here
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestCARFile(t *testing.T) {
	dir := t.TempDir()
	v1 := filepath.Join(dir, "export.car")
	want := []string{"root block", "second block", "third block"}
	cs := writeCAR(t, v1, want...)
	v2 := filepath.Join(dir, "export-v2")
	if err := carv2.WrapV1File(v1, v2); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{v1, v2} {
		t.Run(filepath.Base(p), func(t *testing.T) {
			if !isCAR(p) {
				t.Fatal("not seen as a CAR")
			}
			r, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Kind() != KindCAR || Describe(r) != "CAR" {
				t.Errorf("kind %q, %q", r.Kind(), Describe(r))
			}
			var n int
			err = r.ForEachBlock(context.Background(), func(h multihash.Multihash, size int) error {
				data, err := r.Get(context.Background(), h)
				if err != nil {
					return err
				}
				if len(data) != size || !bytes.Equal(h, cs[n].Hash()) || !bytes.Equal(data, []byte(want[n])) {
					return fmt.Errorf("block %d: %q", n, data)
				}
				n++
				return nil
			})
			if err != nil || n != 3 {
				t.Fatalf("%d blocks: %v", n, err)
			}
			pins, _ := r.Pins(context.Background())
			want := "export"
			if p == v2 {
				want = "export-v2"
			}
			if len(pins) != 1 || !pins[0].Root.Equals(cs[0]) || pins[0].Mode != Recursive || pins[0].Name != want {
				t.Errorf("pins %+v", pins)
			}
			if _, ok, _ := r.MFSRoot(context.Background()); ok {
				t.Error("a CAR has no MFS root")
			}
			if _, err := r.Get(context.Background(), multihash.Multihash("nope")); err != ErrNotFound {
				t.Errorf("missing block: %v", err)
			}
		})
	}
}

func TestCARDirectory(t *testing.T) {
	dir := t.TempDir()
	a := writeCAR(t, filepath.Join(dir, "b.car"), "shared", "only in b")
	b := writeCAR(t, filepath.Join(dir, "sub", "a.car"), "only in a", "shared")
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a car"), 0o644)

	if p, err := Discover(dir); err != nil || p != dir {
		t.Fatalf("Discover = %q, %v", p, err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var n int
	r.ForEachBlock(context.Background(), func(multihash.Multihash, int) error { n++; return nil })
	if n != 3 {
		t.Errorf("%d blocks, want 3 (one shared)", n)
	}
	pins, _ := r.Pins(context.Background())
	if len(pins) != 2 || pins[0].Name != "b" || !pins[0].Root.Equals(a[0]) || pins[1].Name != "sub/a" || !pins[1].Root.Equals(b[0]) {
		t.Errorf("pins %+v", pins)
	}
}

func TestCARNotFSRepo(t *testing.T) {
	// A kubo repo holding a stray .car file is still a kubo repo.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("{}"), 0o644)
	writeCAR(t, filepath.Join(dir, "x.car"), "block")
	if isCAR(dir) {
		t.Error("an fs-repo with a .car in it read as CARs")
	}
	if isCAR(filepath.Join(dir, "config")) {
		t.Error("a JSON file read as a CAR")
	}
}
