package repo

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	"github.com/multiformats/go-multihash"
	"github.com/syndtr/goleveldb/leveldb"
)

// makeHelia writes src's blocks, pins and MFS root the way a Node app's
// Helia stores them (see helia.go): blockstore-fs under <dir>/blockstore and
// a datastore-fs or datastore-level datastore under <dir>/datastore.
func makeHelia(t *testing.T, src Repo, level bool) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, b []byte) {
		t.Helper()
		must(os.MkdirAll(filepath.Dir(p), 0o755))
		must(os.WriteFile(p, b, 0o644))
	}
	must(src.ForEachBlock(ctx, func(h multihash.Multihash, _ int) error {
		data, err := src.Get(ctx, h)
		if err != nil {
			return err
		}
		write(filepath.Join(dir, "blockstore", heliaName(h)), data)
		return nil
	}))
	put := func(key string, b []byte) { write(filepath.Join(dir, "datastore", filepath.FromSlash(key[1:])+".data"), b) }
	if level {
		db, err := leveldb.OpenFile(filepath.Join(dir, "datastore"), nil)
		must(err)
		defer db.Close()
		put = func(key string, b []byte) { must(db.Put([]byte(key), b, nil)) }
	}
	pins, err := src.Pins(ctx)
	must(err)
	for _, p := range pins {
		c := p.Root
		if c.Version() == 0 {
			c = cid.NewCidV1(c.Type(), c.Hash())
		}
		k, err := c.StringOfBase(multibase.Base36)
		must(err)
		rec := map[string]any{"depth": 0, "metadata": map[string]any{}}
		if p.Mode == Recursive {
			rec["depth"] = math.Inf(1)
		}
		if p.Name != "" {
			rec["metadata"] = map[string]any{"name": p.Name}
		}
		b, err := cborg.Marshal(rec)
		must(err)
		put("/pin/"+k, b)
		// What @helia/utils also keeps, which the reader skips.
		put("/pinned-block/"+k, []byte{0xa0})
	}
	if mfs, ok, _ := src.MFSRoot(ctx); ok {
		put("/locals/filesroot", mfs.Bytes())
	}
	return dir
}

func TestHelia(t *testing.T) {
	ctx := context.Background()
	src, err := Open(filepath.Join(extractFixture(t, filepath.Join(fixtureDir, "v18.tar.gz")), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	type pinKey struct {
		mh   string
		mode PinMode
		name string
	}
	keys := func(ps []Pin) []pinKey {
		var out []pinKey
		for _, p := range ps {
			out = append(out, pinKey{string(p.Root.Hash()), p.Mode, p.Name})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].mh < out[j].mh })
		return out
	}
	list := func(r Repo) []string {
		var out []string
		r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
			if data, err := r.Get(ctx, h); err != nil || len(data) != size {
				t.Errorf("block %s: %v", h.B58String(), err)
			}
			out = append(out, string(h))
			return nil
		})
		sort.Strings(out)
		return out
	}
	wantPins, _ := src.Pins(ctx)
	wantMFS, _, _ := src.MFSRoot(ctx)
	wantBlocks := list(src)

	for _, level := range []bool{false, true} {
		dir := makeHelia(t, src, level)
		// The store's directory, or its blockstore with the datastore beside it.
		for _, p := range []string{dir, filepath.Join(dir, "blockstore")} {
			if got, err := Discover(p); err != nil || got != p {
				t.Fatalf("Discover(%s) = %q, %v", p, got, err)
			}
			r, err := Open(p)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind() != KindHelia || Describe(r) != "Helia blockstore and datastore" {
				t.Errorf("opened as %s", Describe(r))
			}
			pins, err := r.Pins(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(wantPins) < 2 || !reflect.DeepEqual(keys(pins), keys(wantPins)) {
				t.Errorf("level=%v: pins\n got %v\nwant %v", level, pins, wantPins)
			}
			if m, ok, err := r.MFSRoot(ctx); err != nil || !ok || !m.Equals(wantMFS) {
				t.Errorf("level=%v: MFS root %s %v %v", level, m, ok, err)
			}
			if got := list(r); !reflect.DeepEqual(got, wantBlocks) {
				t.Errorf("level=%v: %d blocks, want %d", level, len(got), len(wantBlocks))
			}
			r.Close()
		}
	}

	// A blockstore alone: blocks, no pins.
	dir := makeHelia(t, src, false)
	os.RemoveAll(filepath.Join(dir, "datastore"))
	r, err := Open(filepath.Join(dir, "blockstore"))
	if err != nil {
		t.Fatal(err)
	}
	if pins, _ := r.Pins(ctx); len(pins) != 0 || Describe(r) != "Helia blockstore, no datastore beside it" || len(list(r)) != len(wantBlocks) {
		t.Errorf("blockstore alone: %s, %d pins", Describe(r), len(pins))
	}
	// A kubo repo is not taken for a Helia store.
	for _, p := range []string{src.Path(), filepath.Join(src.Path(), "blocks")} {
		if _, ok := heliaBlockstore(p); ok {
			t.Errorf("kubo's %s read as Helia", p)
		}
	}
}
