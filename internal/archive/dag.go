package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"

	chunker "github.com/ipfs/boxo/chunker"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	h "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	uio "github.com/ipfs/boxo/ipld/unixfs/io"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
)

// Every node the archive makes is CIDv1 dag-pb (or raw leaves) with sha2-256.
var builder = cid.V1Builder{Codec: cid.DagProtobuf, MhType: multihash.SHA2_256}

// shardAt is the dag-pb block size past which a directory becomes a HAMT. It
// is pinned here, not taken from boxo's globals, so data/ keeps its CID across
// library upgrades.
var shardAt = 256 << 10

// memDAG holds the blocks the archive makes: data/, index/, explorer/ and the root.
type memDAG map[string]format.Node

func (m memDAG) Get(_ context.Context, c cid.Cid) (format.Node, error) {
	if n, ok := m[string(c.Hash())]; ok {
		return n, nil
	}
	return nil, format.ErrNotFound{Cid: c}
}

func (m memDAG) GetMany(ctx context.Context, cs []cid.Cid) <-chan *format.NodeOption {
	ch := make(chan *format.NodeOption, len(cs))
	for _, c := range cs {
		n, err := m.Get(ctx, c)
		ch <- &format.NodeOption{Node: n, Err: err}
	}
	close(ch)
	return ch
}

func (m memDAG) Add(_ context.Context, n format.Node) error {
	m[string(n.Cid().Hash())] = n
	return nil
}

func (m memDAG) AddMany(ctx context.Context, ns []format.Node) error {
	for _, n := range ns {
		m.Add(ctx, n)
	}
	return nil
}

func (m memDAG) Remove(context.Context, cid.Cid) error       { return nil }
func (m memDAG) RemoveMany(context.Context, []cid.Cid) error { return nil }

// linkNode wraps a repo block so a directory can link to it. Link sizes come
// from the block itself: for dag-pb the cumulative size it records, otherwise
// its length.
func linkNode(c cid.Cid, data []byte) (format.Node, error) {
	b, err := blocks.NewBlockWithCid(data, c)
	if err != nil {
		return nil, err
	}
	if c.Type() == cid.DagProtobuf {
		return merkledag.DecodeProtobufBlock(b)
	}
	return &merkledag.RawNode{Block: b}, nil
}

type child struct {
	name string
	node format.Node
}

// mkdir makes a UnixFS directory, sharded when large, and stores it in dag.
func mkdir(ctx context.Context, dag memDAG, kids []child) (format.Node, error) {
	d, err := uio.NewDirectory(dag, uio.WithCidBuilder(builder),
		uio.WithSizeEstimationMode(uio.SizeEstimationBlock), uio.WithMaxHAMTFanout(256))
	if err != nil {
		return nil, err
	}
	d.SetHAMTShardingSize(shardAt)
	sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
	for _, k := range kids {
		if err := d.AddChild(ctx, k.name, k.node); err != nil {
			return nil, err
		}
	}
	n, err := d.GetNode()
	if err != nil {
		return nil, err
	}
	return n, dag.Add(ctx, n)
}

// mkfile makes a UnixFS file with 256 KiB raw leaves.
func mkfile(dag memDAG, data []byte) (format.Node, error) {
	p := h.DagBuilderParams{Maxlinks: h.DefaultLinksPerBlock, RawLeaves: true, CidBuilder: builder, Dagserv: dag}
	db, err := p.New(chunker.NewSizeSplitter(bytes.NewReader(data), 256<<10))
	if err != nil {
		return nil, err
	}
	return balanced.Layout(db)
}

// importDir adds a directory tree from disk.
func importDir(ctx context.Context, dag memDAG, dir string) (format.Node, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var kids []child
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		var n format.Node
		if e.IsDir() {
			n, err = importDir(ctx, dag, p)
		} else {
			var b []byte
			if b, err = os.ReadFile(p); err == nil {
				n, err = mkfile(dag, b)
			}
		}
		if err != nil {
			return nil, err
		}
		kids = append(kids, child{e.Name(), n})
	}
	return mkdir(ctx, dag, kids)
}
