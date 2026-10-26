package archive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	chunker "github.com/ipfs/boxo/chunker"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	h "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

// fakeRepo is an in-memory repo.Repo. Its blocks map iterates in random
// order, which the archive must not depend on.
type fakeRepo struct {
	dupes  bool // list every block twice, as repos 5 to 11 can
	blocks map[string][]byte
	pins   []repo.Pin
	mfs    cid.Cid
}

func newFake() *fakeRepo { return &fakeRepo{blocks: map[string][]byte{}} }

func (f *fakeRepo) Path() string          { return "/fake" }
func (f *fakeRepo) Version() repo.Version { return 18 }
func (f *fakeRepo) Close() error          { return nil }
func (f *fakeRepo) Pins(context.Context) ([]repo.Pin, error) {
	return f.pins, nil
}
func (f *fakeRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	return f.mfs, f.mfs.Defined(), nil
}
func (f *fakeRepo) ForEachBlock(_ context.Context, fn func(multihash.Multihash, int) error) error {
	for k, v := range f.blocks {
		if err := fn(multihash.Multihash(k), len(v)); err != nil {
			return err
		}
		if f.dupes {
			if err := fn(multihash.Multihash(k), len(v)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (f *fakeRepo) Get(_ context.Context, h multihash.Multihash) ([]byte, error) {
	if b, ok := f.blocks[string(h)]; ok {
		return b, nil
	}
	return nil, repo.ErrNotFound
}

// Add stores n and, through the DAG service interface, everything boxo adds.
func (f *fakeRepo) Add(_ context.Context, n format.Node) error {
	f.blocks[string(n.Cid().Hash())] = n.RawData()
	return nil
}
func (f *fakeRepo) AddMany(ctx context.Context, ns []format.Node) error {
	for _, n := range ns {
		f.Add(ctx, n)
	}
	return nil
}
func (f *fakeRepo) dag() format.DAGService { return fakeDAG{f} }

type fakeDAG struct{ *fakeRepo }

func (d fakeDAG) Get(ctx context.Context, c cid.Cid) (format.Node, error) {
	b, err := d.fakeRepo.Get(ctx, c.Hash())
	if err != nil {
		return nil, err
	}
	return linkNode(c, b)
}
func (d fakeDAG) GetMany(context.Context, []cid.Cid) <-chan *format.NodeOption { return nil }
func (d fakeDAG) Remove(context.Context, cid.Cid) error                        { return nil }
func (d fakeDAG) RemoveMany(context.Context, []cid.Cid) error                  { return nil }

// file adds data as kubo would: CIDv0, 1 KiB chunks, so tests get many blocks.
func (f *fakeRepo) file(t *testing.T, data []byte, rawLeaves bool) format.Node {
	t.Helper()
	p := h.DagBuilderParams{Maxlinks: 8, RawLeaves: rawLeaves, Dagserv: f.dag()}
	if rawLeaves {
		p.CidBuilder = builder
	}
	db, err := p.New(chunker.NewSizeSplitter(bytes.NewReader(data), 1024))
	if err != nil {
		t.Fatal(err)
	}
	n, err := balanced.Layout(db)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *fakeRepo) dir(t *testing.T, kids map[string]format.Node) format.Node {
	t.Helper()
	d := merkledag.NodeWithData([]byte{0x08, 0x01}) // UnixFS directory
	for name, k := range kids {
		if err := d.AddNodeLink(name, k); err != nil {
			t.Fatal(err)
		}
	}
	f.Add(context.Background(), d)
	return d
}

func random(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func testOptions() Options {
	return Options{MinPiece: 8 << 10, MaxPiece: 320 << 10, CutBits: 2, Orphans: true, Tool: "ipfs2fil test",
		Now: func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) }}
}

// sample makes a repo with an MFS tree of files, two recursive pins, a direct
// pin, and an orphan tree.
func sample(t *testing.T, edit bool) *fakeRepo {
	f := newFake()
	files := map[string]format.Node{}
	for i := range 40 {
		data := random(int64(i), 6000)
		if edit && i == 20 {
			copy(data[3000:], "edited")
		}
		files[string(rune('a'+i%26))+string(rune('0'+i/26))] = f.file(t, data, i%2 == 0)
	}
	f.mfs = f.dir(t, files).Cid()
	p1 := f.file(t, random(100, 20000), false)
	p2 := f.dir(t, map[string]format.Node{"x": f.file(t, random(101, 3000), true)})
	d := f.file(t, random(102, 500), true)
	f.pins = []repo.Pin{{Root: p2.Cid(), Mode: repo.Recursive, Name: "site"}, {Root: p1.Cid(), Mode: repo.Recursive},
		{Root: d.Cid(), Mode: repo.Direct}}
	f.dir(t, map[string]format.Node{"lost": f.file(t, random(103, 4000), false)}) // orphan tree
	f.Add(context.Background(), merkledag.NewRawNode([]byte("a loose block")))
	return f
}

func build(t *testing.T, r repo.Repo, dir string) *Result {
	t.Helper()
	res, err := Build(context.Background(), r, dir, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// check reads every CAR, checks every block's hash, and walks the archive
// root over the CARs alone.
func check(t *testing.T, res *Result) {
	t.Helper()
	missing := map[string]bool{}
	for _, c := range res.Missing {
		m, _ := cid.Decode(c)
		missing[string(m.Hash())] = true
	}
	got := map[string][]byte{}
	root, _ := cid.Decode(res.Manifest.Root)
	for i, p := range res.Pieces {
		f, err := os.Open(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		pc, n, err := car.PieceCID(f)
		if err != nil || pc.String() != p.PieceCID || n != p.CARBytes {
			t.Fatalf("piece %d: PieceCID %s (%d bytes), manifest says %s (%d)", i, pc, n, p.PieceCID, p.CARBytes)
		}
		f.Seek(0, 0)
		cr, err := car.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		first := true
		for {
			c, data, err := cr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := hashOK(c, data); !ok || err != nil {
				t.Fatalf("block %s in piece %d fails its hash", c, i)
			}
			if first && !cr.Root.Equals(c) && !cr.Root.Equals(root) {
				t.Fatalf("piece %d header names %s, neither its first block %s nor the archive root", i, cr.Root, c)
			}
			first = false
			got[string(c.Hash())] = data
		}
		f.Close()
	}
	seen := map[string]bool{}
	var walk func(c cid.Cid)
	walk = func(c cid.Cid) {
		if seen[string(c.Hash())] || isIdentity(c) || missing[string(c.Hash())] {
			return
		}
		seen[string(c.Hash())] = true
		data, ok := got[string(c.Hash())]
		if !ok {
			t.Fatalf("%s is reachable from the root but in no piece", c)
		}
		ls, err := links(c, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			walk(l)
		}
	}
	walk(root)
	if len(seen) != len(got) {
		t.Fatalf("%d blocks reachable, %d in pieces", len(seen), len(got))
	}
}

func dataPieces(res *Result) map[string]bool {
	out := map[string]bool{}
	for _, p := range res.Pieces {
		if p.BlockList != "" {
			out[p.PieceCID] = true
		}
	}
	return out
}

func TestBuild(t *testing.T) {
	r := sample(t, false)
	dir := t.TempDir()
	opts := testOptions()
	last := map[string][2]int64{}
	opts.Progress = func(stage string, done, total int64) {
		if p := last[stage]; done < p[0] || (total != 0 && done > total) {
			t.Errorf("progress %s went %d/%d after %d/%d", stage, done, total, p[0], p[1])
		}
		last[stage] = [2]int64{done, total}
	}
	res, err := Build(context.Background(), r, dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	check(t, res)
	var dataBytes int64
	for _, p := range res.Pieces {
		if p.BlockList != "" {
			dataBytes += p.CARBytes
		}
	}
	if w := last["write"]; w[0] != dataBytes || w[1] != dataBytes || last["walk"][1] != int64(len(r.blocks)) {
		t.Errorf("final progress %v, data pieces hold %d bytes", last, dataBytes)
	}
	// the archive's index/ and explorer/ stay on disk for local preview
	for _, p := range []string{"index/manifest.json", "index/pieces/0000", "explorer/index.html", ManifestFile} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Error(err)
		}
	}
	m := res.Manifest
	if len(dataPieces(res)) < 8 {
		t.Fatalf("only %d data pieces; the test needs more to mean anything", len(dataPieces(res)))
	}
	kinds := map[string]int{}
	for _, rt := range m.Roots {
		kinds[rt.Kind]++
		if rt.FirstPiece < 0 || rt.LastPiece < rt.FirstPiece || rt.Blocks == 0 {
			t.Errorf("root %+v", rt)
		}
	}
	// one orphan tree and one loose block; the tree's children are not roots
	want := map[string]int{manifest.KindPinRecursive: 2, manifest.KindPinDirect: 1, manifest.KindMFS: 1, manifest.KindOrphan: 2}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%d %s roots, want %d", kinds[k], k, n)
		}
	}
	if m.Source.Blocks != int64(len(r.blocks)) || len(res.Missing) != 0 {
		t.Errorf("source %+v, missing %v", m.Source, res.Missing)
	}
	for _, p := range res.Pieces {
		if p.CARBytes > testOptions().MaxPiece {
			t.Errorf("piece %d is %d bytes", p.Index, p.CARBytes)
		}
	}
}

func TestRerunIsByteIdentical(t *testing.T) {
	dir := t.TempDir()
	a := build(t, sample(t, false), dir)
	b := build(t, sample(t, false), t.TempDir()) // fresh output dir
	c := build(t, sample(t, false), dir)         // same dir, reads its last manifest
	for _, res := range []*Result{b, c} {
		if res.Manifest.Root != a.Manifest.Root || len(res.Pieces) != len(a.Pieces) {
			t.Fatalf("root %s vs %s, %d vs %d pieces", res.Manifest.Root, a.Manifest.Root, len(res.Pieces), len(a.Pieces))
		}
		for i := range a.Pieces {
			x, _ := os.ReadFile(a.Pieces[i].Path)
			y, _ := os.ReadFile(res.Pieces[i].Path)
			if !bytes.Equal(x, y) {
				t.Fatalf("piece %d differs", i)
			}
		}
		if len(res.Manifest.Snapshots) != 1 {
			t.Fatalf("%d snapshots after an unchanged re-run", len(res.Manifest.Snapshots))
		}
	}
}

// TestPushThenRepack is pack, push (the store writes Copies into the
// manifest), pack: the second pack must give the same pieces, so a re-run
// uploads nothing, and must keep the archive key Snapshots[0].Root.
func TestPushThenRepack(t *testing.T) {
	dir := t.TempDir()
	a := build(t, sample(t, false), dir)
	m, err := manifest.Read(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]string{}
	for _, p := range m.Pieces {
		ids[p.Index] = fmt.Sprint(100 + p.Index)
	}
	m.Copies = []manifest.Copy{{Provider: "f0123", DataSetID: "7", PieceIDs: ids, Service: "foc-fwss"}}
	if err := m.Write(filepath.Join(dir, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	b := build(t, sample(t, false), dir)
	if len(b.Pieces) != len(a.Pieces) {
		t.Fatalf("%d pieces, then %d", len(a.Pieces), len(b.Pieces))
	}
	for i := range a.Pieces {
		if a.Pieces[i].PieceCID != b.Pieces[i].PieceCID {
			t.Errorf("piece %d changed after a push", i)
		}
	}
	if s := b.Manifest.Snapshots; len(s) != 1 || s[0].Root != a.Manifest.Root || len(b.Manifest.Copies) != 1 {
		t.Fatalf("snapshots %+v, copies %d", s, len(b.Manifest.Copies))
	}
}

func TestEditChangesNearbyPieces(t *testing.T) {
	dir := t.TempDir()
	a := build(t, sample(t, false), dir)
	b := build(t, sample(t, true), dir)
	check(t, b)
	old, now := dataPieces(a), dataPieces(b)
	changed := 0
	for p := range now {
		if !old[p] {
			changed++
		}
	}
	// the edited file's pieces, the piece holding the new MFS root, and at most
	// one piece before the cuts line up again
	t.Logf("%d of %d data pieces changed", changed, len(now))
	if changed == 0 || changed > 4 {
		t.Fatalf("%d of %d data pieces changed after a one-file edit", changed, len(now))
	}
	m := b.Manifest
	if len(m.Snapshots) != 2 || m.Snapshots[0].Root != a.Manifest.Root || m.Snapshots[1].Root != m.Root {
		t.Fatalf("snapshots %+v", m.Snapshots)
	}
	if n := len(m.Snapshots[1].NewPieces); n < changed {
		t.Fatalf("snapshot 1 lists %d new pieces, %d data pieces changed", n, changed)
	}
	for _, p := range m.Pieces {
		if old[p.PieceCID] != (p.Snapshot == 0) {
			t.Errorf("piece %d: snapshot %d", p.Index, p.Snapshot)
		}
	}
}

func TestDuplicateListings(t *testing.T) {
	a := build(t, sample(t, false), t.TempDir())
	r := sample(t, false)
	r.dupes = true
	b := build(t, r, t.TempDir())
	check(t, b)
	if b.Manifest.Root != a.Manifest.Root || b.Manifest.Source != a.Manifest.Source || len(b.Manifest.Roots) != len(a.Manifest.Roots) {
		t.Fatalf("listing blocks twice changed the archive: %+v vs %+v", b.Manifest.Source, a.Manifest.Source)
	}
}

func TestMissingAndSharded(t *testing.T) {
	defer func(n int) { shardAt = n }(shardAt)
	shardAt = 1024
	f := newFake()
	for i := range 60 {
		n := merkledag.NewRawNode(random(int64(i), 200))
		f.Add(context.Background(), n)
		f.pins = append(f.pins, repo.Pin{Root: n.Cid(), Mode: repo.Recursive})
	}
	file := f.file(t, random(7, 5000), false)
	gone := file.Links()[2].Cid
	delete(f.blocks, string(gone.Hash()))
	f.pins = append(f.pins, repo.Pin{Root: file.Cid(), Mode: repo.Recursive})
	res := build(t, f, t.TempDir())
	if len(res.Missing) != 1 || res.Missing[0] != gone.String() {
		t.Fatalf("missing %v, want %s", res.Missing, gone)
	}
	check(t, res)
	// pins/recursive outgrew shardAt, so it is a HAMT
	got := map[string][]byte{}
	for _, p := range res.Pieces {
		f, _ := os.Open(p.Path)
		cr, _ := car.NewReader(f)
		for {
			c, data, err := cr.Next()
			if err != nil {
				break
			}
			got[c.String()] = data
		}
		f.Close()
	}
	node := func(c cid.Cid) *merkledag.ProtoNode {
		n, err := merkledag.DecodeProtobuf(got[c.String()])
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	root, _ := cid.Decode(res.Manifest.Root)
	at := func(n *merkledag.ProtoNode, name string) *merkledag.ProtoNode {
		l, err := n.GetNodeLink(name)
		if err != nil {
			t.Fatalf("no %s: %v", name, err)
		}
		return node(l.Cid)
	}
	rec := at(at(at(node(root), "data"), "pins"), "recursive")
	fs, err := unixfs.FSNodeFromBytes(rec.Data())
	if err != nil || fs.Type() != unixfs.THAMTShard {
		t.Fatalf("pins/recursive is %v (%v), want a HAMT shard", fs, err)
	}
}

func TestEntries(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	sub := f.dir(t, map[string]format.Node{"a.md": f.file(t, []byte("# hi"), true)})
	site := f.dir(t, map[string]format.Node{"photo.jpg": f.file(t, random(1, 3000), false), "sub": sub})
	// a sharded directory, as kubo makes for big folders
	d, err := uio.NewHAMTDirectory(f.dag(), 0, uio.WithMaxHAMTFanout(8))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if err := d.AddChild(ctx, fmt.Sprintf("n%02d.txt", i), f.file(t, []byte(fmt.Sprint(i)), true)); err != nil {
			t.Fatal(err)
		}
	}
	big, err := d.GetNode()
	if err != nil {
		t.Fatal(err)
	}
	f.Add(ctx, big)
	f.pins = []repo.Pin{{Root: site.Cid(), Mode: repo.Recursive, Name: "site"}}
	f.mfs = big.Cid()

	l := newLayout(f)
	roots, err := l.walkRoots(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.cut(testOptions()); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{}
	for _, rt := range roots {
		m.Roots = append(m.Roots, l.rootEntry(rt))
	}
	got := map[string]explorer.Entry{}
	for _, e := range l.entries(roots, m) {
		got[e.Path] = e
	}
	sp := "pins/recursive/" + site.Cid().String()
	want := map[string]string{sp: "folder", sp + "/photo.jpg": "image", sp + "/sub": "folder", sp + "/sub/a.md": "text",
		"mfs": "folder", "mfs/n07.txt": "text", "mfs/n39.txt": "text"}
	for p, k := range want {
		if got[p].Kind != k {
			t.Errorf("%s: kind %q, want %q", p, got[p].Kind, k)
		}
	}
	if len(got) != 5+40 {
		t.Errorf("%d entries, want 45", len(got))
	}
	if got[sp].Name != "site" || got[sp+"/photo.jpg"].Bytes != 3000 || got[sp].Bytes != 3004 || got["mfs"].Bytes != 70 {
		t.Errorf("site %+v, photo %+v, mfs %+v", got[sp], got[sp+"/photo.jpg"], got["mfs"])
	}
}

func TestCutHere(t *testing.T) {
	h, _ := multihash.Encode(append(make([]byte, 30), 0x04, 0x00), multihash.SHA2_256)
	for bits, want := range map[int]bool{0: true, 8: true, 10: true, 11: false, 16: false} {
		if cutHere(h, bits) != want {
			t.Errorf("cutHere(..0400, %d) != %v", bits, want)
		}
	}
}

func TestCBORLinks(t *testing.T) {
	c := merkledag.NewRawNode([]byte("x")).Cid()
	cb := append([]byte{0}, c.Bytes()...)
	// {"a": [1, 42(h'00..cid')], "b": "hi"}
	b := []byte{0xa2, 0x61, 'a', 0x82, 0x01, 0xd8, 0x2a, 0x58, byte(len(cb))}
	b = append(b, cb...)
	b = append(b, 0x61, 'b', 0x62, 'h', 'i')
	h, _ := multihash.Sum(b, multihash.SHA2_256, -1)
	got := guess(h, b)
	if got.Type() != cid.DagCBOR {
		t.Fatalf("guessed codec %x", got.Type())
	}
	ls, err := links(got, b)
	if err != nil || len(ls) != 1 || !ls[0].Equals(c) {
		t.Fatalf("links %v %v", ls, err)
	}
	if _, err := links(got, b[:len(b)-1]); err == nil {
		t.Fatal("truncated dag-cbor should not parse")
	}
}

// sharedFrom is a Shared lookup over the data pieces of res, built in dir.
func sharedFrom(t *testing.T, dir string, res *Result) func(multihash.Multihash) (SharedBlock, bool) {
	t.Helper()
	held := map[string]SharedBlock{}
	for _, p := range res.Manifest.Pieces {
		if p.BlockList == "" {
			continue
		}
		sb := SharedBlock{Archive: res.Manifest.Snapshots[0].Root, PieceCID: p.PieceCID}
		err := ReadBlockList(filepath.Join(dir, "index", p.BlockList), func(h multihash.Multihash) error {
			held[string(h)] = sb
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return func(h multihash.Multihash) (SharedBlock, bool) {
		sb, ok := held[string(h)]
		return sb, ok
	}
}

// TestSharedBlocks packs repo A, then repo B (A plus a new pin) with A's
// pieces as the shared lookup: B stores only what A lacks, every block of B
// is in B's or A's pieces, and re-packing B gives the same pieces.
func TestSharedBlocks(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := build(t, sample(t, false), dirA)

	repoB := func() *fakeRepo {
		r := sample(t, false)
		extra := r.file(t, random(200, 30000), false)
		r.pins = append(r.pins, repo.Pin{Root: extra.Cid(), Mode: repo.Recursive, Name: "new"})
		return r
	}
	opts := testOptions()
	opts.Shared = sharedFrom(t, dirA, a)
	b, err := Build(context.Background(), repoB(), dirB, opts)
	if err != nil {
		t.Fatal(err)
	}
	sh := b.Manifest.Shared
	if sh == nil || sh.Blocks == 0 || len(sh.Pieces) == 0 {
		t.Fatalf("nothing shared: %+v", sh)
	}
	inA := map[string]bool{}
	for _, p := range a.Pieces {
		if p.BlockList != "" {
			inA[p.PieceCID] = true
		}
	}
	for _, p := range sh.Pieces {
		if !inA[p.PieceCID] || p.Archive != a.Manifest.Snapshots[0].Root {
			t.Errorf("shared piece %+v is not one of A's data pieces", p)
		}
	}
	// B's data pieces hold only the new pin's blocks.
	var dataB int
	for _, p := range b.Pieces {
		if p.BlockList != "" {
			dataB++
		}
	}
	if dataB == 0 || dataB >= len(dataPieces(a)) {
		t.Fatalf("B has %d data pieces, A %d", dataB, len(dataPieces(a)))
	}
	if sh.Blocks+int64(sumBlocks(b)) < b.Manifest.Source.Blocks {
		t.Errorf("shared %d + own %d < %d repo blocks", sh.Blocks, sumBlocks(b), b.Manifest.Source.Blocks)
	}
	// Every block reachable from B's root is in B's pieces or A's.
	both := &Result{Manifest: b.Manifest, Pieces: append(append([]Piece{}, b.Pieces...), a.Pieces[:len(dataPieces(a))]...)}
	checkCovered(t, both)

	// Re-pack B: same pieces. Once B is stored, a lookup that now claims
	// everything still leaves B's own blocks alone; before, it may shrink B.
	again, err := Build(context.Background(), repoB(), dirB, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Pieces) != len(b.Pieces) {
		t.Fatalf("%d pieces, then %d", len(b.Pieces), len(again.Pieces))
	}
	for i := range b.Pieces {
		if b.Pieces[i].PieceCID != again.Pieces[i].PieceCID {
			t.Errorf("piece %d changed on re-pack", i)
		}
	}
	greedyOpts := opts
	greedyOpts.Shared = func(multihash.Multihash) (SharedBlock, bool) { return SharedBlock{Archive: "x", PieceCID: "y"}, true }
	if g, err := Build(context.Background(), repoB(), t.TempDir(), greedyOpts); err != nil || len(g.Pieces) >= len(b.Pieces) {
		t.Fatalf("an unstored B did not shrink: %v", err)
	}
	m, err := manifest.Read(filepath.Join(dirB, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]string{}
	for _, p := range m.Pieces {
		ids[p.Index] = fmt.Sprint(p.Index + 1)
	}
	m.Copies = []manifest.Copy{{Provider: "1", DataSetID: "9", PieceIDs: ids}}
	if err := m.Write(filepath.Join(dirB, ManifestFile)); err != nil {
		t.Fatal(err)
	}
	opts = greedyOpts
	greedy, err := Build(context.Background(), repoB(), dirB, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < dataB; i++ {
		if greedy.Pieces[i].PieceCID != b.Pieces[i].PieceCID {
			t.Errorf("data piece %d changed when other archives grew", i)
		}
	}
}

func sumBlocks(res *Result) int {
	n := 0
	for _, p := range res.Pieces {
		if p.BlockList != "" {
			n += int(p.Blocks)
		}
	}
	return n
}

// checkCovered walks the archive root over the given pieces' CARs.
func checkCovered(t *testing.T, res *Result) {
	t.Helper()
	got := map[string][]byte{}
	for _, p := range res.Pieces {
		f, err := os.Open(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		cr, err := car.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		for {
			c, data, err := cr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got[string(c.Hash())] = data
		}
		f.Close()
	}
	seen := map[string]bool{}
	var walk func(c cid.Cid)
	walk = func(c cid.Cid) {
		if seen[string(c.Hash())] || isIdentity(c) {
			return
		}
		seen[string(c.Hash())] = true
		data, ok := got[string(c.Hash())]
		if !ok {
			t.Fatalf("%s is reachable from the root but in no piece", c)
		}
		ls, _ := links(c, data)
		for _, l := range ls {
			walk(l)
		}
	}
	root, _ := cid.Decode(res.Manifest.Root)
	walk(root)
}

// TestRecord: the record goes first in the archive's own pieces, which name it
// as their second root; data pieces are unchanged.
func TestRecord(t *testing.T) {
	plain := build(t, sample(t, false), t.TempDir())
	opts := testOptions()
	opts.Record = []byte("ipfs2fil test record")
	res, err := Build(context.Background(), sample(t, false), t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := RecordCID(opts.Record)
	first := true
	for i, p := range res.Pieces {
		f, err := os.Open(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := car.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		c, data, _ := r.Next()
		f.Close()
		if p.BlockList != "" {
			if len(r.Roots) != 1 || p.PieceCID != plain.Pieces[i].PieceCID {
				t.Errorf("data piece %d: roots %v, changed: %v", i, r.Roots, p.PieceCID != plain.Pieces[i].PieceCID)
			}
			continue
		}
		if len(r.Roots) != 2 || r.Root.String() != res.Manifest.Root || !r.Roots[1].Equals(rec) {
			t.Errorf("own piece %d: roots %v", i, r.Roots)
		}
		if first && (!c.Equals(rec) || string(data) != string(opts.Record)) {
			t.Errorf("own piece %d starts with %s", i, c)
		}
		first = false
	}
	if res.Manifest.Root != plain.Manifest.Root {
		t.Errorf("the record changed the archive root")
	}
}

// TestOddLinkNames packs a pin whose directory names links "", ".", "..",
// "a/b" and "x" twice, as some DAGs in the wild do. Each gets its own clean
// path and the archive, explorer included, still builds.
func TestOddLinkNames(t *testing.T) {
	f := newFake()
	d := merkledag.NodeWithData([]byte{0x08, 0x01}) // UnixFS directory
	for i, name := range []string{"", ".", "..", "a/b", "x", "x", "%2E"} {
		if err := d.AddNodeLink(name, f.file(t, []byte(fmt.Sprint("file ", i)), true)); err != nil {
			t.Fatal(err)
		}
	}
	f.Add(context.Background(), d)
	f.pins = []repo.Pin{{Root: d.Cid(), Mode: repo.Recursive}}
	res := build(t, f, t.TempDir())
	check(t, res)

	l := newLayout(f)
	roots, err := l.walkRoots(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{}
	for _, rt := range roots {
		m.Roots = append(m.Roots, l.rootEntry(rt))
	}
	sp := "pins/recursive/" + d.Cid().String()
	got := map[string]string{}
	for _, e := range l.entries(roots, m) {
		got[e.Path] = e.Name
	}
	want := map[string]string{sp + "/(no name)": "(no name)", sp + "/%2E~2": ".", sp + "/%2E%2E": "..",
		sp + "/a%2Fb": "a/b", sp + "/x": "x", sp + "/x~2": "x", sp + "/%2E": "%2E"} // links sort by name, so "%2E" comes first
	for p, n := range want {
		if g, ok := got[p]; !ok || g != n {
			t.Errorf("%s: name %q (listed %v), want %q", p, g, ok, n)
		}
	}
	if len(got) != len(want)+1 {
		t.Errorf("entries %v", got)
	}
}
