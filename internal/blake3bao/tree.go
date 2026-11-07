package blake3bao

import (
	"bytes"
	"context"
	"fmt"
	"io"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
)

// Encode reads size bytes from r and emits the blob's blocks, children before
// parents, and returns the root CID. Memory use is one group plus the tree
// depth. codec is CodecRaw or CodecHashSeq.
func Encode(codec uint64, r io.Reader, size uint64, emit func(blocks.Block) error) (cid.Cid, error) {
	if codec == CodecHashSeq && size%DigestSize != 0 {
		return cid.Undef, fmt.Errorf("blake3-bao: hashseq of %d bytes is not a multiple of 32", size)
	}
	e := &encoder{codec: codec, r: r, emit: emit, buf: make([]byte, GroupSize)}
	h, err := e.node(0, size, true)
	if err != nil {
		return cid.Undef, err
	}
	return CID(codec, h), nil
}

// EncodeBytes is Encode over a byte slice, returning the blocks too.
func EncodeBytes(codec uint64, data []byte) (cid.Cid, []blocks.Block, error) {
	var out []blocks.Block
	c, err := Encode(codec, bytes.NewReader(data), uint64(len(data)), func(b blocks.Block) error {
		out = append(out, b)
		return nil
	})
	return c, out, err
}

type encoder struct {
	codec uint64
	r     io.Reader
	emit  func(blocks.Block) error
	buf   []byte
}

func (e *encoder) put(n Node) ([32]byte, error) {
	h := n.Hash()
	b, err := blocks.NewBlockWithCid(n.Encode(), CID(e.codec, h))
	if err != nil {
		return h, err
	}
	return h, e.emit(b)
}

// node encodes the subtree of n bytes starting at chunk start.
func (e *encoder) node(start, n uint64, root bool) ([32]byte, error) {
	if n <= GroupSize {
		p := e.buf[:n]
		if _, err := io.ReadFull(e.r, p); err != nil {
			return [32]byte{}, err
		}
		k := KindLeaf
		if root {
			k = KindRootLeaf
		}
		return e.put(Node{Kind: k, StartChunk: start, Data: p})
	}
	mid := leftLen(n)
	l, err := e.node(start, mid, false)
	if err != nil {
		return l, err
	}
	r, err := e.node(start+mid/ChunkSize, n-mid, false)
	if err != nil {
		return r, err
	}
	k := KindParent
	if root {
		k = KindRootParent
	}
	return e.put(Node{Kind: k, Left: l, Right: r})
}

// Getter fetches a block by CID. Callers need not verify it; the walk does.
type Getter func(ctx context.Context, c cid.Cid) ([]byte, error)

// Size finds a blob's size by walking the right edge of its tree (one block
// per level). iroh carries the size beside the hash; this is for when only
// the CID is known.
func Size(ctx context.Context, root cid.Cid, get Getter) (uint64, error) {
	c := root
	for depth := 0; depth < 64; depth++ {
		n, err := fetch(ctx, get, c)
		if err != nil {
			return 0, err
		}
		if (depth == 0) != n.IsRoot() {
			return 0, fmt.Errorf("%w: root flag on wrong node", ErrShape)
		}
		if !n.IsParent() {
			return n.StartChunk*ChunkSize + uint64(len(n.Data)), nil
		}
		c = CID(c.Type(), n.Right)
	}
	return 0, fmt.Errorf("%w: tree too deep", ErrShape)
}

// Read writes bytes [off, off+length) of the blob to w, fetching only the
// blocks that cover the range and checking every block and the tree shape.
// A full read proves root == BLAKE3(content) == the iroh hash.
func Read(ctx context.Context, root cid.Cid, size uint64, get Getter, w io.Writer, off, length uint64) error {
	if off > size {
		return fmt.Errorf("blake3-bao: offset %d past size %d", off, size)
	}
	if length > size-off {
		length = size - off
	}
	d := &decoder{ctx: ctx, codec: root.Type(), get: get, w: w, lo: off, hi: off + length}
	return d.node(root, 0, size, true)
}

type decoder struct {
	ctx    context.Context
	codec  uint64
	get    Getter
	w      io.Writer
	lo, hi uint64 // byte range to write
}

// node checks the block for the subtree of n bytes at chunk start against the
// shape BLAKE3 gives a blob of this size, and writes its part of [lo, hi).
func (d *decoder) node(c cid.Cid, start, n uint64, root bool) error {
	n0 := start * ChunkSize
	nd, err := fetch(d.ctx, d.get, c)
	if err != nil {
		return err
	}
	if nd.IsRoot() != root {
		return fmt.Errorf("%w: root flag on wrong node", ErrShape)
	}
	if n <= GroupSize {
		if nd.IsParent() {
			return fmt.Errorf("%w: parent where a %d-byte leaf belongs", ErrShape, n)
		}
		if nd.StartChunk != start || uint64(len(nd.Data)) != n {
			return fmt.Errorf("%w: leaf at chunk %d with %d bytes, want chunk %d with %d", ErrShape, nd.StartChunk, len(nd.Data), start, n)
		}
		lo, hi := max(d.lo, n0), min(d.hi, n0+n)
		if lo < hi {
			_, err = d.w.Write(nd.Data[lo-n0 : hi-n0])
		}
		return err
	}
	if !nd.IsParent() {
		return fmt.Errorf("%w: leaf where a %d-byte subtree belongs", ErrShape, n)
	}
	mid := leftLen(n)
	if d.lo < n0+mid && d.hi > n0 {
		if err := d.node(CID(d.codec, nd.Left), start, mid, false); err != nil {
			return err
		}
	}
	if d.hi > n0+mid && d.lo < n0+n {
		if err := d.node(CID(d.codec, nd.Right), start+mid/ChunkSize, n-mid, false); err != nil {
			return err
		}
	}
	return nil
}

func fetch(ctx context.Context, get Getter, c cid.Cid) (Node, error) {
	b, err := get(ctx, c)
	if err != nil {
		return Node{}, err
	}
	if err := Verify(c, b); err != nil {
		return Node{}, fmt.Errorf("%s: %w", c, err)
	}
	return Parse(b)
}

// Outboard returns iroh's pre-order outboard for the blob (bao-tree with
// 16 KiB groups, no size prefix): the bodies of its parent blocks in
// pre-order. Blobs of one group have an empty outboard.
func Outboard(ctx context.Context, root cid.Cid, get Getter) ([]byte, error) {
	var out []byte
	var rec func(c cid.Cid) error
	rec = func(c cid.Cid) error {
		n, err := fetch(ctx, get, c)
		if err != nil || !n.IsParent() {
			return err
		}
		out = append(out, n.Left[:]...)
		out = append(out, n.Right[:]...)
		if err := rec(CID(c.Type(), n.Left)); err != nil {
			return err
		}
		return rec(CID(c.Type(), n.Right))
	}
	return out, rec(root)
}

// FromOutboard builds a blob's blocks from its data and an iroh pre-order
// outboard, without recomputing parent hashes from scratch: each parent block
// is taken from the outboard, and each leaf is hashed and checked against it.
// This is how an iroh store would export.
func FromOutboard(codec uint64, irohHash [32]byte, data, outboard []byte, emit func(blocks.Block) error) error {
	size := uint64(len(data))
	pos := 0
	var rec func(start, n uint64, want [32]byte, root bool) error
	rec = func(start, n uint64, want [32]byte, root bool) error {
		var nd Node
		if n <= GroupSize {
			nd = Node{Kind: KindLeaf, StartChunk: start, Data: data[start*ChunkSize : start*ChunkSize+n]}
			if root {
				nd.Kind = KindRootLeaf
			}
		} else {
			if pos+64 > len(outboard) {
				return fmt.Errorf("blake3-bao: outboard too short")
			}
			nd = Node{Kind: KindParent}
			if root {
				nd.Kind = KindRootParent
			}
			copy(nd.Left[:], outboard[pos:])
			copy(nd.Right[:], outboard[pos+32:])
			pos += 64
		}
		h := nd.Hash()
		if h != want {
			return ErrMismatch
		}
		b, err := blocks.NewBlockWithCid(nd.Encode(), CID(codec, h))
		if err != nil {
			return err
		}
		if nd.IsParent() {
			mid := leftLen(n)
			if err := rec(start, mid, nd.Left, false); err != nil {
				return err
			}
			if err := rec(start+mid/ChunkSize, n-mid, nd.Right, false); err != nil {
				return err
			}
		}
		return emit(b)
	}
	if err := rec(0, size, irohHash, true); err != nil {
		return err
	}
	if pos != len(outboard) {
		return fmt.Errorf("blake3-bao: outboard has %d extra bytes", len(outboard)-pos)
	}
	return nil
}
