//go:build ignore

// Prototype for ipfs2fil: read a kubo repo (fs-repo 11..18) offline and read-only,
// collect its roots, link them under one UnixFS directory, pack every reachable block
// into CARs no bigger than a target size, compute each CAR's piece CID, and verify.
// Prototype kept for reference; see prototype/README.md. Build it from its own module in project files.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"encoding/binary"
	commcid "github.com/filecoin-project/go-fil-commcid"
	commp "github.com/filecoin-project/go-fil-commp-hashhash"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	dshelp "github.com/ipfs/boxo/datastore/dshelp"
	"github.com/ipfs/boxo/exchange/offline"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	"github.com/ipfs/boxo/pinning/pinner/dspinner"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	ds "github.com/ipfs/go-datastore"
	dsq "github.com/ipfs/go-datastore/query"
	flatfs "github.com/ipfs/go-ds-flatfs"
	leveldb "github.com/ipfs/go-ds-leveldb"
	format "github.com/ipfs/go-ipld-format"
	carv2 "github.com/ipld/go-car/v2"
	mh "github.com/multiformats/go-multihash"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

type repo struct {
	path    string
	version string
	blocks  *flatfs.Datastore
	meta    *leveldb.Datastore
	// every block in the flatfs, by multihash (codec lost for repo >= 12)
	have map[string][]byte // mh bytes -> raw key bytes
}

func openRepo(path string) (*repo, error) {
	v, err := os.ReadFile(filepath.Join(path, "version"))
	if err != nil {
		return nil, err
	}
	spec, _ := os.ReadFile(filepath.Join(path, "datastore_spec"))
	if !strings.Contains(string(spec), "flatfs") || !strings.Contains(string(spec), "levelds") {
		return nil, fmt.Errorf("prototype only reads flatfs+levelds, spec is %s", spec)
	}
	bs, err := flatfs.Open(filepath.Join(path, "blocks"), false)
	if err != nil {
		return nil, err
	}
	md, err := leveldb.NewDatastore(filepath.Join(path, "datastore"), (*leveldb.Options)(&opt.Options{ReadOnly: true}))
	if err != nil {
		return nil, fmt.Errorf("leveldb (is a daemon running? copy the dir first): %w", err)
	}
	return &repo{path: path, version: strings.TrimSpace(string(v)), blocks: bs, meta: md}, nil
}

// keyed by multihash so repo 11 (CID keys) and repo 12+ (multihash keys) look the same
type mhStore struct {
	r     *repo
	keys  map[string]ds.Key // mh -> flatfs key
	v1cid map[string]cid.Cid
}

func (r *repo) index(ctx context.Context) (*mhStore, error) {
	s := &mhStore{r: r, keys: map[string]ds.Key{}, v1cid: map[string]cid.Cid{}}
	res, err := r.blocks.Query(ctx, dsq.Query{KeysOnly: true})
	if err != nil {
		return nil, err
	}
	for e := range res.Next() {
		if e.Error != nil {
			return nil, e.Error
		}
		k := ds.NewKey(e.Key)
		b, err := dshelp.BinaryFromDsKey(k)
		if err != nil {
			return nil, fmt.Errorf("bad key %s: %w", e.Key, err)
		}
		if c, err := cid.Cast(b); err == nil {
			s.keys[string(c.Hash())] = k // repo 11: CID key
			s.v1cid[string(c.Hash())] = c
		} else if m, err := mh.Cast(b); err == nil {
			s.keys[string(m)] = k // repo 12+: multihash key
		} else {
			return nil, fmt.Errorf("key %s is neither CID nor multihash", e.Key)
		}
	}
	return s, nil
}

// blockstore.Blockstore-ish read side over the repo's flatfs, verifying hashes
func (s *mhStore) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	k, ok := s.keys[string(c.Hash())]
	if !ok {
		return nil, format.ErrNotFound{Cid: c}
	}
	data, err := s.r.blocks.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	chk, err := c.Prefix().Sum(data)
	if err != nil || !chk.Equals(c) {
		return nil, fmt.Errorf("block %s fails its hash (corrupt repo?)", c)
	}
	return blocks.NewBlockWithCid(data, c)
}

// minimal blockstore so boxo's dag service works on top
type bstore struct {
	*mhStore
	extra map[string]blocks.Block // new blocks we make (aggregate root)
}

func (b *bstore) Get(ctx context.Context, c cid.Cid) (blocks.Block, error) {
	if x, ok := b.extra[c.KeyString()]; ok {
		return x, nil
	}
	return b.mhStore.Get(ctx, c)
}
func (b *bstore) Has(ctx context.Context, c cid.Cid) (bool, error) {
	if _, ok := b.extra[c.KeyString()]; ok {
		return true, nil
	}
	_, ok := b.keys[string(c.Hash())]
	return ok, nil
}
func (b *bstore) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	x, err := b.Get(ctx, c)
	if err != nil {
		return 0, err
	}
	return len(x.RawData()), nil
}
func (b *bstore) Put(ctx context.Context, x blocks.Block) error {
	b.extra[x.Cid().KeyString()] = x
	return nil
}
func (b *bstore) PutMany(ctx context.Context, xs []blocks.Block) error {
	for _, x := range xs {
		b.Put(ctx, x)
	}
	return nil
}
func (b *bstore) DeleteBlock(context.Context, cid.Cid) error { return fmt.Errorf("read only") }
func (b *bstore) AllKeysChan(context.Context) (<-chan cid.Cid, error) {
	return nil, fmt.Errorf("unused")
}
func (b *bstore) HashOnRead(bool) {}

var _ blockstore.Blockstore = (*bstore)(nil)

type rootSet struct {
	Recursive []cid.Cid
	Direct    []cid.Cid
	MFS       *cid.Cid
	Orphans   []cid.Cid
}

func main() {
	repoPath := flag.String("repo", "", "kubo repo path")
	out := flag.String("out", "out", "output dir")
	target := flag.Int64("piece", 1<<20, "target CAR size in bytes")
	orphans := flag.Bool("orphans", true, "include blocks no root reaches")
	flag.Parse()
	ctx := context.Background()

	r, err := openRepo(*repoPath)
	must(err)
	s, err := r.index(ctx)
	must(err)
	fmt.Printf("repo version %s, %d blocks in flatfs\n", r.version, len(s.keys))

	bs := &bstore{mhStore: s, extra: map[string]blocks.Block{}}
	dag := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))

	// roots: pins (dspinner, repo >= 11) and the MFS root
	pinner, err := dspinner.New(ctx, readOnly{r.meta}, dag)
	must(err)
	var roots rootSet
	for p := range pinner.RecursiveKeys(ctx, false) {
		must(p.Err)
		roots.Recursive = append(roots.Recursive, p.Pin.Key)
	}
	for p := range pinner.DirectKeys(ctx, false) {
		must(p.Err)
		roots.Direct = append(roots.Direct, p.Pin.Key)
	}
	if v, err := r.meta.Get(ctx, ds.NewKey("/local/filesroot")); err == nil {
		c, err := cid.Cast(v)
		must(err)
		roots.MFS = &c
	}

	// what the roots reach
	reached := map[string]bool{}
	var walk func(c cid.Cid, recurse bool) error
	walk = func(c cid.Cid, recurse bool) error {
		if reached[string(c.Hash())] {
			return nil
		}
		reached[string(c.Hash())] = true
		if !recurse {
			return nil
		}
		n, err := dag.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("%s: %w", c, err)
		}
		for _, l := range n.Links() {
			if err := walk(l.Cid, true); err != nil {
				return err
			}
		}
		return nil
	}
	missing := 0
	for _, c := range roots.Recursive {
		if err := walk(c, true); err != nil {
			fmt.Println("incomplete pin:", err)
			missing++
		}
	}
	for _, c := range roots.Direct {
		walk(c, false)
	}
	if roots.MFS != nil {
		if err := walk(*roots.MFS, true); err != nil {
			fmt.Println("incomplete MFS:", err)
		}
	}
	// orphans: blocks no root reaches. For repo >= 12 the codec is guessed.
	if *orphans {
		var ks []string
		for k := range s.keys {
			if !reached[k] {
				ks = append(ks, k)
			}
		}
		sort.Strings(ks)
		for _, k := range ks {
			roots.Orphans = append(roots.Orphans, s.guess(ctx, k))
		}
	}
	fmt.Printf("roots: %d recursive, %d direct, mfs=%v, %d orphans (of which reachable from each other not deduped)\n",
		len(roots.Recursive), len(roots.Direct), roots.MFS != nil, len(roots.Orphans))

	// aggregate root: one UnixFS directory linking everything
	root, err := aggregate(ctx, dag, r, roots)
	must(err)
	fmt.Println("aggregate root:", root)

	must(os.MkdirAll(*out, 0o755))
	pieces, err := pack(ctx, bs, dag, root, *out, *target)
	must(err)
	man := map[string]any{"root": root.String(), "repoVersion": r.version, "pieces": pieces}
	j, _ := json.MarshalIndent(man, "", "  ")
	must(os.WriteFile(filepath.Join(*out, "manifest.json"), j, 0o644))
	for _, p := range pieces {
		fmt.Printf("  %s  %8d bytes  %4d blocks  %s\n", p.File, p.Size, p.Blocks, p.PieceCID)
	}
	must(verify(ctx, root, *out, pieces))
}

// read-only wrapper: the pinner must never write to a user's repo
type readOnly struct{ ds.Batching }

func (readOnly) Put(context.Context, ds.Key, []byte) error { return fmt.Errorf("read only") }
func (readOnly) Delete(context.Context, ds.Key) error      { return fmt.Errorf("read only") }
func (r readOnly) Batch(ctx context.Context) (ds.Batch, error) {
	return nil, fmt.Errorf("read only")
}

func (s *mhStore) guess(ctx context.Context, k string) cid.Cid {
	if c, ok := s.v1cid[k]; ok {
		return c // repo 11 kept the CID
	}
	m := mh.Multihash(k)
	data, _ := s.r.blocks.Get(ctx, s.keys[k])
	if n, err := merkledag.DecodeProtobuf(data); err == nil {
		if bytes.Equal(n.RawData(), data) {
			return cid.NewCidV1(cid.DagProtobuf, m)
		}
	}
	return cid.NewCidV1(cid.Raw, m)
}

func aggregate(ctx context.Context, dag format.DAGService, r *repo, roots rootSet) (cid.Cid, error) {
	mk := func(name string, cs []cid.Cid) (format.Node, error) {
		d, err := uio.NewDirectory(dag) // switches to a HAMT when big
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			n, err := linkTo(ctx, dag, c)
			if err != nil {
				return nil, err
			}
			if err := d.AddChild(ctx, c.String(), n); err != nil {
				return nil, err
			}
		}
		return d.GetNode()
	}
	top, _ := uio.NewDirectory(dag)
	pins, _ := uio.NewDirectory(dag)
	for _, x := range []struct {
		n  string
		cs []cid.Cid
	}{{"recursive", roots.Recursive}, {"direct", roots.Direct}} {
		n, err := mk(x.n, x.cs)
		if err != nil {
			return cid.Undef, err
		}
		must(dag.Add(ctx, n))
		must(pins.AddChild(ctx, x.n, n))
	}
	pn, _ := pins.GetNode()
	must(dag.Add(ctx, pn))
	must(top.AddChild(ctx, "pins", pn))
	if roots.MFS != nil {
		n, err := linkTo(ctx, dag, *roots.MFS)
		if err != nil {
			return cid.Undef, err
		}
		must(top.AddChild(ctx, "mfs", n))
	}
	if len(roots.Orphans) > 0 {
		n, err := mk("orphans", roots.Orphans)
		if err != nil {
			return cid.Undef, err
		}
		must(dag.Add(ctx, n))
		must(top.AddChild(ctx, "orphans", n))
	}
	info, _ := json.Marshal(map[string]any{"tool": "ipfs2fil-proto", "repoVersion": r.version})
	f := merkledag.NodeWithData(unixfs.FilePBData(info, uint64(len(info))))
	must(dag.Add(ctx, f))
	must(top.AddChild(ctx, "ipfs2fil.json", f))
	tn, err := top.GetNode()
	if err != nil {
		return cid.Undef, err
	}
	must(dag.Add(ctx, tn))
	return tn.Cid(), nil
}

// a node to link to; for a direct pin or orphan we only need its CID and size
func linkTo(ctx context.Context, dag format.DAGService, c cid.Cid) (format.Node, error) {
	n, err := dag.Get(ctx, c)
	if err != nil {
		return nil, err
	}
	return n, nil
}

type piece struct {
	File     string   `json:"file"`
	Size     int64    `json:"size"`
	Blocks   int      `json:"blocks"`
	PieceCID string   `json:"pieceCid"`
	Roots    []string `json:"roots"`
}

// DFS from the root; each CAR holds blocks in walk order and is cut before it passes target.
// Blocks shared by two subtrees go in the first CAR that needs them (dedupe).
func pack(ctx context.Context, bs *bstore, dag format.DAGService, root cid.Cid, out string, target int64) ([]piece, error) {
	var order []cid.Cid
	seen := map[string]bool{}
	var walk func(c cid.Cid) error
	walk = func(c cid.Cid) error {
		if seen[string(c.Hash())] {
			return nil
		}
		seen[string(c.Hash())] = true
		order = append(order, c)
		if c.Type() == cid.Raw {
			return nil
		}
		n, err := dag.Get(ctx, c)
		if err != nil {
			return err
		}
		for _, l := range n.Links() {
			if err := walk(l.Cid); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	var pieces []piece
	var cur []blocks.Block
	var curSize int64
	hdr := int64(100) // CARv1 header with one root, roughly
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		name := fmt.Sprintf("piece-%04d.car", len(pieces))
		p, err := writeCar(filepath.Join(out, name), cur)
		if err != nil {
			return err
		}
		p.File = name
		pieces = append(pieces, p)
		cur, curSize = nil, 0
		return nil
	}
	for _, c := range order {
		b, err := bs.Get(ctx, c)
		if err != nil {
			return nil, err
		}
		sz := int64(len(b.RawData()) + len(c.Bytes()) + 10)
		if curSize+sz+hdr > target && len(cur) > 0 {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		cur = append(cur, b)
		curSize += sz
	}
	return pieces, flush()
}

func writeCar(path string, bl []blocks.Block) (piece, error) {
	f, err := os.Create(path)
	if err != nil {
		return piece{}, err
	}
	defer f.Close()
	cp := &commp.Calc{}
	w := bufio.NewWriter(io.MultiWriter(f, cp))
	// CARv1: header root = this CAR's first block (gateways ignore it; it names the piece)
	if err := writeCarV1(w, bl); err != nil {
		return piece{}, err
	}
	must(w.Flush())
	st, _ := f.Stat()
	raw, _, err := cp.Digest()
	if err != nil {
		return piece{}, err
	}
	pc, err := commcid.DataCommitmentToPieceCidv2(raw, uint64(st.Size()))
	if err != nil {
		return piece{}, err
	}
	return piece{Size: st.Size(), Blocks: len(bl), PieceCID: pc.String(), Roots: []string{bl[0].Cid().String()}}, nil
}

func writeCarV1(w io.Writer, bl []blocks.Block) error {
	if err := ldWrite(w, carHeader(bl[0].Cid())); err != nil {
		return err
	}
	for _, b := range bl {
		if err := ldWrite(w, b.Cid().Bytes(), b.RawData()); err != nil {
			return err
		}
	}
	return nil
}

func verify(ctx context.Context, root cid.Cid, out string, pieces []piece) error {
	// load every block from every CAR, check each hash, then walk the root over only those
	got := map[string]blocks.Block{}
	for _, p := range pieces {
		f, err := os.Open(filepath.Join(out, p.File))
		if err != nil {
			return err
		}
		br, err := carv2.NewBlockReader(f)
		if err != nil {
			return err
		}
		for {
			b, err := br.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			chk, err := b.Cid().Prefix().Sum(b.RawData())
			if err != nil || !chk.Equals(b.Cid()) {
				return fmt.Errorf("bad block %s in %s", b.Cid(), p.File)
			}
			got[string(b.Cid().Hash())] = b
		}
		f.Close()
	}
	n := 0
	var walk func(c cid.Cid) error
	seen := map[string]bool{}
	walk = func(c cid.Cid) error {
		if seen[string(c.Hash())] {
			return nil
		}
		seen[string(c.Hash())] = true
		b, ok := got[string(c.Hash())]
		if !ok {
			return fmt.Errorf("verify: %s missing from every CAR", c)
		}
		n++
		if c.Type() == cid.Raw {
			return nil
		}
		nd, err := format.Decode(b, func(b blocks.Block) (format.Node, error) { return merkledag.DecodeProtobufBlock(b) })
		if err != nil {
			return err
		}
		for _, l := range nd.Links() {
			if err := walk(l.Cid); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return err
	}
	fmt.Printf("verified: %d blocks across %d CARs, every block reachable from %s present and hash-checked\n", n, len(pieces), root)
	return nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// CARv1 header as dag-cbor: {"roots": [cid], "version": 1}
func carHeader(c cid.Cid) []byte {
	cb := append([]byte{0}, c.Bytes()...)
	var h []byte
	h = append(h, 0xa2, 0x65)
	h = append(h, "roots"...)
	h = append(h, 0x81, 0xd8, 0x2a)
	if len(cb) < 24 {
		h = append(h, 0x40|byte(len(cb)))
	} else {
		h = append(h, 0x58, byte(len(cb)))
	}
	h = append(h, cb...)
	h = append(h, 0x67)
	h = append(h, "version"...)
	h = append(h, 0x01)
	return h
}

func ldWrite(w io.Writer, parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	var buf [binary.MaxVarintLen64]byte
	if _, err := w.Write(buf[:binary.PutUvarint(buf[:], uint64(n))]); err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := w.Write(p); err != nil {
			return err
		}
	}
	return nil
}
