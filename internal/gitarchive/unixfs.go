package gitarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	chunker "github.com/ipfs/boxo/chunker"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/boxo/ipld/unixfs/importer/balanced"
	h "github.com/ipfs/boxo/ipld/unixfs/importer/helpers"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
)

// LeafSize is the raw leaf size of stored packs and indexes: big enough that
// a pack is few blocks, small enough for every gateway and bitswap peer.
const LeafSize = 1 << 20

var fileBuilder = cid.V1Builder{Codec: cid.DagProtobuf, MhType: multihash.SHA2_256}

// BlockSink receives each block as it is made.
type BlockSink func(c cid.Cid, data []byte) error

// sinkDAG is a write-only DAGService that hands blocks to a sink.
type sinkDAG struct{ put BlockSink }

func (s sinkDAG) Get(_ context.Context, c cid.Cid) (format.Node, error) {
	return nil, format.ErrNotFound{Cid: c}
}
func (s sinkDAG) GetMany(context.Context, []cid.Cid) <-chan *format.NodeOption {
	ch := make(chan *format.NodeOption)
	close(ch)
	return ch
}
func (s sinkDAG) Add(_ context.Context, n format.Node) error { return s.put(n.Cid(), n.RawData()) }
func (s sinkDAG) AddMany(ctx context.Context, ns []format.Node) error {
	for _, n := range ns {
		if err := s.Add(ctx, n); err != nil {
			return err
		}
	}
	return nil
}
func (s sinkDAG) Remove(context.Context, cid.Cid) error       { return nil }
func (s sinkDAG) RemoveMany(context.Context, []cid.Cid) error { return nil }

// putFile stores r as a UnixFS file with LeafSize raw leaves.
func putFile(r io.Reader, put BlockSink) (cid.Cid, error) {
	p := h.DagBuilderParams{Maxlinks: h.DefaultLinksPerBlock, RawLeaves: true, CidBuilder: fileBuilder, Dagserv: sinkDAG{put}}
	db, err := p.New(chunker.NewSizeSplitter(r, LeafSize))
	if err != nil {
		return cid.Undef, err
	}
	n, err := balanced.Layout(db)
	if err != nil {
		return cid.Undef, err
	}
	return n.Cid(), nil
}

// BlockGetter returns a block's bytes; it must have checked them against the CID.
type BlockGetter func(c cid.Cid) ([]byte, error)

// maxFileDepth bounds how deep readFile follows dag-pb links.
const maxFileDepth = 64

// readFile writes the bytes of a UnixFS file to w. It reads only what its
// own writer made: dag-pb files and raw leaves.
func readFile(get BlockGetter, c cid.Cid, w io.Writer) (int64, error) {
	return readFileAt(get, c, w, 0)
}

func readFileAt(get BlockGetter, c cid.Cid, w io.Writer, depth int) (int64, error) {
	if depth > maxFileDepth {
		return 0, errors.New("file DAG is too deep")
	}
	data, err := get(c)
	if err != nil {
		return 0, err
	}
	switch c.Type() {
	case cid.Raw:
		n, err := w.Write(data)
		return int64(n), err
	case cid.DagProtobuf:
	default:
		return 0, fmt.Errorf("block %s: not part of a UnixFS file", c)
	}
	b, err := blocks.NewBlockWithCid(data, c)
	if err != nil {
		return 0, err
	}
	pn, err := merkledag.DecodeProtobufBlock(b)
	if err != nil {
		return 0, err
	}
	fsn, err := unixfs.FSNodeFromBytes(pn.(*merkledag.ProtoNode).Data())
	if err != nil {
		return 0, err
	}
	if t := fsn.Type(); t != unixfs.TFile && t != unixfs.TRaw {
		return 0, fmt.Errorf("block %s is a UnixFS %v, not a file", c, t)
	}
	var total int64
	if d := fsn.Data(); len(d) > 0 {
		n, err := w.Write(d)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	for _, l := range pn.Links() {
		n, err := readFileAt(get, l.Cid, w, depth+1)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// readAll reads a small UnixFS file into memory.
func readAll(get BlockGetter, c cid.Cid) ([]byte, error) {
	var b bytes.Buffer
	_, err := readFile(get, c, &b)
	return b.Bytes(), err
}
