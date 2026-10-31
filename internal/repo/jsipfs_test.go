package repo

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/syndtr/goleveldb/leveldb"
)

// makeJSRepo writes a js-ipfs repo of version v holding src's blocks, pins
// and MFS root, laid out the way ipfs-repo 14 and ipfs-repo-migrations 13
// write it (see jsipfs.go). Repos 7 and 8 keep their pins as a pin DAG, so
// src must have one (kubo repos 3-10).
func makeJSRepo(t *testing.T, src *fsRepo, v Version) string {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".jsipfs")
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
	write(filepath.Join(dir, "version"), []byte(strconv.Itoa(int(v))))
	write(filepath.Join(dir, "config"), []byte(`{"Identity":{"PeerID":"12D3KooWtest"},"Datastore":{"Spec":{"type":"mount"}}}`))
	write(filepath.Join(dir, "blocks", "SHARDING.data"), []byte("/repo/flatfs/shard/v1/next-to-last/2"))
	write(filepath.Join(dir, "blocks", "_README.data"), []byte("This is a repository of IPLD objects."))
	shard := shardNextToLast(2)
	must(src.forEachKey(ctx, func(key string, mh multihash.Multihash, _ int) error {
		data, err := src.ds.Get(key)
		if err != nil {
			return err
		}
		name := b32.EncodeToString(mh)
		if v <= 7 {
			// Keys were CIDs then; the source's own key is one.
			name = key[len(src.blockPrefix):]
		}
		write(filepath.Join(dir, "blocks", shard(name), name+".data"), data)
		return nil
	}))

	ds, err := leveldb.OpenFile(filepath.Join(dir, "datastore"), nil)
	must(err)
	defer ds.Close()
	mfs, hasMFS, err := src.MFSRoot(ctx)
	must(err)
	if hasMFS {
		if v >= 11 {
			must(ds.Put([]byte("/local/filesroot"), mfs.Bytes(), nil))
		} else {
			write(filepath.Join(dir, "local", "filesroot"), mfs.Bytes())
		}
	}
	if v <= 8 {
		root, err := src.ds.Get("/local/pins")
		must(err)
		must(ds.Put([]byte("/local/pins"), root, nil))
		return dir
	}
	pins, err := src.Pins(ctx)
	must(err)
	pdb, err := leveldb.OpenFile(filepath.Join(dir, "pins"), nil)
	must(err)
	defer pdb.Close()
	for _, p := range pins {
		rec := map[string]any{"depth": 0}
		if p.Mode == Recursive {
			rec["depth"] = math.Inf(1)
		}
		if p.Root.Version() != 0 {
			rec["version"] = p.Root.Version()
		}
		if p.Root.Type() != cid.DagProtobuf {
			rec["codec"] = p.Root.Type()
		}
		b, err := cborg.Marshal(rec)
		must(err)
		must(pdb.Put([]byte("/"+b32.EncodeToString(p.Root.Hash())), b, nil))
	}
	return dir
}

// cborg, which js-ipfs encodes pins with, writes the shortest float that
// keeps the value: Infinity is the half float f9 7c00.
var cborg, _ = cbor.EncOptions{ShortestFloat: cbor.ShortestFloat16, InfConvert: cbor.InfConvertFloat16}.EncMode()

func TestJSIPFS(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		from string
		v    Version
	}{{"v07", 7}, {"v07", 8}, {"v18", 9}, {"v18", 10}, {"v18", 11}, {"v18", 12}} {
		t.Run(fmt.Sprintf("%s-js%d", tc.from, tc.v), func(t *testing.T) {
			s, err := openFS(filepath.Join(extractFixture(t, filepath.Join(fixtureDir, tc.from+".tar.gz")), "repo"), map[string]Version{"v07": 7, "v18": 18}[tc.from])
			if err != nil {
				t.Fatal(err)
			}
			src := s.(*fsRepo)
			defer src.Close()
			dir := makeJSRepo(t, src, tc.v)

			r, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Kind() != KindJSIPFS || r.Version() != tc.v {
				t.Fatalf("opened as %s %d", Describe(r), r.Version())
			}
			wantPins, _ := src.Pins(ctx)
			for i := range wantPins {
				wantPins[i].Name = "" // js-ipfs has no pin names
			}
			gotPins, err := r.Pins(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(wantPins) < 2 || !reflect.DeepEqual(gotPins, wantPins) {
				t.Errorf("pins\n got %v\nwant %v", gotPins, wantPins)
			}
			wm, _, _ := src.MFSRoot(ctx)
			gm, ok, err := r.MFSRoot(ctx)
			if err != nil || !ok || !gm.Equals(wm) {
				t.Errorf("MFS root %s %v %v, want %s", gm, ok, err, wm)
			}
			list := func(r Repo) []string {
				var out []string
				r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
					data, err := r.Get(ctx, h)
					if err != nil || len(data) != size {
						t.Errorf("block %s: %v", h.B58String(), err)
					}
					out = append(out, string(h))
					return nil
				})
				sort.Strings(out)
				return out
			}
			if got, want := list(r), list(src); len(want) == 0 || !reflect.DeepEqual(got, want) {
				t.Errorf("%d blocks, want %d", len(got), len(want))
			}
		})
	}
}

func TestJSIPFSDetection(t *testing.T) {
	// Kubo repos 6 and later have a datastore_spec; js-ipfs repos never do.
	for _, name := range []string{"v07", "v12", "v18"} {
		dir := filepath.Join(extractFixture(t, filepath.Join(fixtureDir, name+".tar.gz")), "repo")
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind() != KindFSRepo {
			t.Errorf("%s opened as %s", name, Describe(r))
		}
		r.Close()
	}
}
