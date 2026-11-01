// Package blake3bao is a prototype of the blake3-bao IPLD codec: a way to
// store a BLAKE3-addressed blob of any size (an iroh blob) as small IPLD
// blocks, where the root CID's digest is the blob's plain BLAKE3 hash, which
// is the iroh hash.
//
// The spec is docs/specs/blake3-bao.md (a draft). In short, every block is one node of
// the BLAKE3 tree, cut at 16 KiB chunk groups (iroh's block size):
//
//	kind 0x00  leaf          uvarint(startChunk) || data   (1..16384 bytes)
//	kind 0x01  root leaf     data                          (0..16384 bytes)
//	kind 0x02  parent        leftCV(32) || rightCV(32)
//	kind 0x03  root parent   leftCV(32) || rightCV(32)
//
// The blake3-bao multihash of a block is the BLAKE3 chaining value of the node
// it frames: a leaf's chunks are compressed with counters from startChunk, a
// parent is compressed with the PARENT flag, and root kinds add the ROOT flag.
// A root block's digest is therefore BLAKE3(blob). Everything a hash needs is
// in the block, so any block can be checked alone, like any IPLD block.
//
// The codes below are private-use placeholders until codes are registered in
// the multicodec table.
package blake3bao

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math/bits"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"lukechampine.com/blake3/guts"
)

// Placeholder codes (multicodec private-use range), pending registration.
const (
	// CodecRaw is the codec for tree nodes of a blob whose content is opaque
	// bytes (iroh BlobFormat::Raw).
	CodecRaw uint64 = 0x3b3000
	// CodecHashSeq is the codec for tree nodes of a blob whose content is a
	// sequence of 32-byte BLAKE3 hashes (iroh BlobFormat::HashSeq). Its leaves
	// carry links to other blobs.
	CodecHashSeq uint64 = 0x3b3001
	// MhCode is the blake3-bao multihash: the BLAKE3 chaining value of a
	// framed tree node. Digest length is 32.
	MhCode uint64 = 0x3b3020
)

// Tree constants.
const (
	ChunkSize  = guts.ChunkSize        // 1024, fixed by BLAKE3
	ChunkLog   = 4                     // chunks per group = 1<<ChunkLog, as iroh
	GroupSize  = ChunkSize << ChunkLog // 16384
	DigestSize = 32
)

// Block kinds (first byte of every block).
const (
	KindLeaf       byte = 0x00
	KindRootLeaf   byte = 0x01
	KindParent     byte = 0x02
	KindRootParent byte = 0x03

	kindRootBit   = 0x01
	kindParentBit = 0x02
)

var (
	ErrBadBlock = errors.New("blake3-bao: malformed block")
	ErrMismatch = errors.New("blake3-bao: block does not match its CID")
	ErrShape    = errors.New("blake3-bao: tree shape does not match BLAKE3")
)

func init() {
	// Lets cid.Prefix.Sum and multihash.Sum use the blake3-bao function, so
	// ordinary blockstore code can check these blocks.
	mh.Register(MhCode, func() hash.Hash { return &hasher{} })
}

// Node is a parsed block.
type Node struct {
	Kind       byte
	StartChunk uint64 // leaves only; 0 for a root leaf
	Data       []byte // leaves only
	Left       [32]byte
	Right      [32]byte
}

// IsRoot reports whether the node is the root of its blob.
func (n Node) IsRoot() bool { return n.Kind&kindRootBit != 0 }

// IsParent reports whether the node is a parent (two child hashes).
func (n Node) IsParent() bool { return n.Kind&kindParentBit != 0 }

// Parse checks a block's framing and returns its node. It does not hash.
func Parse(b []byte) (Node, error) {
	if len(b) == 0 {
		return Node{}, fmt.Errorf("%w: empty", ErrBadBlock)
	}
	n := Node{Kind: b[0]}
	body := b[1:]
	switch n.Kind {
	case KindParent, KindRootParent:
		if len(body) != 64 {
			return Node{}, fmt.Errorf("%w: parent body is %d bytes, want 64", ErrBadBlock, len(body))
		}
		copy(n.Left[:], body[:32])
		copy(n.Right[:], body[32:])
	case KindRootLeaf:
		if len(body) > GroupSize {
			return Node{}, fmt.Errorf("%w: leaf of %d bytes", ErrBadBlock, len(body))
		}
		n.Data = body
	case KindLeaf:
		start, k := binary.Uvarint(body)
		if k <= 0 {
			return Node{}, fmt.Errorf("%w: bad start chunk varint", ErrBadBlock)
		}
		// Minimal varints only, so each node has exactly one encoding.
		var tmp [binary.MaxVarintLen64]byte
		if binary.PutUvarint(tmp[:], start) != k {
			return Node{}, fmt.Errorf("%w: non-minimal varint", ErrBadBlock)
		}
		if start%(1<<ChunkLog) != 0 {
			return Node{}, fmt.Errorf("%w: start chunk %d not on a group boundary", ErrBadBlock, start)
		}
		n.StartChunk = start
		n.Data = body[k:]
		if len(n.Data) == 0 || len(n.Data) > GroupSize {
			return Node{}, fmt.Errorf("%w: leaf of %d bytes", ErrBadBlock, len(n.Data))
		}
		if start > (1<<54)/ChunkSize { // keep startChunk*1024+len inside 2^64
			return Node{}, fmt.Errorf("%w: start chunk %d too large", ErrBadBlock, start)
		}
	default:
		return Node{}, fmt.Errorf("%w: unknown kind 0x%02x", ErrBadBlock, n.Kind)
	}
	return n, nil
}

// Hash returns the blake3-bao digest of a block: the chaining value of the
// node it frames (the BLAKE3 hash of the blob, for a root block).
func Hash(b []byte) ([32]byte, error) {
	n, err := Parse(b)
	if err != nil {
		return [32]byte{}, err
	}
	return n.Hash(), nil
}

// Hash returns the node's chaining value.
func (n Node) Hash() [32]byte {
	var flags uint32
	if n.IsRoot() {
		flags = guts.FlagRoot
	}
	if n.IsParent() {
		return cvBytes(guts.ChainingValue(guts.ParentNode(words(n.Left), words(n.Right), &guts.IV, flags)))
	}
	return cvBytes(leafCV(n.Data, n.StartChunk, flags))
}

// Encode serialises a node.
func (n Node) Encode() []byte {
	switch n.Kind {
	case KindParent, KindRootParent:
		b := make([]byte, 65)
		b[0] = n.Kind
		copy(b[1:], n.Left[:])
		copy(b[33:], n.Right[:])
		return b
	case KindRootLeaf:
		return append([]byte{n.Kind}, n.Data...)
	default:
		b := make([]byte, 1, 1+binary.MaxVarintLen64+len(n.Data))
		b[0] = n.Kind
		b = binary.AppendUvarint(b, n.StartChunk)
		return append(b, n.Data...)
	}
}

// CID returns the CID of a node digest under codec.
func CID(codec uint64, digest [32]byte) cid.Cid {
	m, err := mh.Encode(digest[:], MhCode)
	if err != nil {
		panic(err) // fixed length; cannot fail
	}
	return cid.NewCidV1(codec, m)
}

// RootCID returns the CID an iroh hash maps to under codec (CodecRaw or
// CodecHashSeq). This is the whole iroh -> IPLD mapping.
func RootCID(codec uint64, irohHash [32]byte) cid.Cid { return CID(codec, irohHash) }

// IrohHash returns the iroh hash (BLAKE3 of the blob) a root CID names.
func IrohHash(c cid.Cid) ([32]byte, error) {
	var h [32]byte
	d, err := mh.Decode(c.Hash())
	if err != nil {
		return h, err
	}
	if d.Code != MhCode || d.Length != DigestSize {
		return h, fmt.Errorf("blake3-bao: not a blake3-bao CID: %s", c)
	}
	copy(h[:], d.Digest)
	return h, nil
}

// Verify checks that b is a well-formed block whose hash matches c.
func Verify(c cid.Cid, b []byte) error {
	codec := c.Type()
	if codec != CodecRaw && codec != CodecHashSeq {
		return fmt.Errorf("blake3-bao: codec 0x%x is not a blake3-bao codec", codec)
	}
	want, err := IrohHash(c)
	if err != nil {
		return err
	}
	n, err := Parse(b)
	if err != nil {
		return err
	}
	if codec == CodecHashSeq && !n.IsParent() && len(n.Data)%DigestSize != 0 {
		return fmt.Errorf("%w: hashseq leaf of %d bytes is not a multiple of 32", ErrBadBlock, len(n.Data))
	}
	if n.Hash() != want {
		return ErrMismatch
	}
	return nil
}

// Links returns the CIDs a block links to. A parent links to its two children
// (same codec). A hashseq leaf links to the root of each blob it names, read
// as CodecRaw: like iroh, a hash sequence does not say its children's format.
func Links(codec uint64, b []byte) ([]cid.Cid, error) {
	n, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if n.IsParent() {
		return []cid.Cid{CID(codec, n.Left), CID(codec, n.Right)}, nil
	}
	if codec != CodecHashSeq {
		return nil, nil
	}
	if len(n.Data)%DigestSize != 0 {
		return nil, fmt.Errorf("%w: hashseq leaf of %d bytes", ErrBadBlock, len(n.Data))
	}
	out := make([]cid.Cid, 0, len(n.Data)/DigestSize)
	for i := 0; i < len(n.Data); i += DigestSize {
		var h [32]byte
		copy(h[:], n.Data[i:])
		out = append(out, RootCID(CodecRaw, h))
	}
	return out, nil
}

// leafCV hashes up to one group of data as the BLAKE3 subtree whose first
// chunk has counter start. guts.CompressBuffer merges up to 16 chunks the way
// BLAKE3 does, which is exactly one group.
func leafCV(p []byte, start uint64, flags uint32) [8]uint32 {
	var buf [guts.MaxSIMD * guts.ChunkSize]byte
	n := copy(buf[:], p)
	node := guts.CompressBuffer(&buf, n, &guts.IV, start, 0)
	node.Flags |= flags
	return guts.ChainingValue(node)
}

// leftLen is the byte length of the left subtree of a BLAKE3 node spanning n
// bytes (n > ChunkSize): the largest power-of-two count of chunks that leaves
// at least one byte on the right.
func leftLen(n uint64) uint64 {
	chunks := (n + ChunkSize - 1) / ChunkSize
	return (uint64(1) << (bits.Len64(chunks-1) - 1)) * ChunkSize
}

func words(b [32]byte) (w [8]uint32) {
	for i := range w {
		w[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return
}

func cvBytes(cv [8]uint32) (b [32]byte) {
	for i, w := range cv {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return
}

// hasher adapts Hash to hash.Hash for the multihash registry. A malformed
// block hashes to 32 zero bytes, which no real node has (that would be a
// BLAKE3 preimage), so it never verifies.
type hasher struct{ buf []byte }

func (h *hasher) Write(p []byte) (int, error) { h.buf = append(h.buf, p...); return len(p), nil }
func (h *hasher) Sum(b []byte) []byte {
	d, err := Hash(h.buf)
	if err != nil {
		d = [32]byte{}
	}
	return append(b, d[:]...)
}
func (h *hasher) Reset()         { h.buf = h.buf[:0] }
func (h *hasher) Size() int      { return DigestSize }
func (h *hasher) BlockSize() int { return GroupSize }
