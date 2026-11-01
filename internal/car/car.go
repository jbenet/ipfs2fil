// Package car writes and reads CARv1 files and computes their PieceCID (v2).
//
// Writing is deterministic: the same root and blocks in the same order give the
// same bytes, so the same PieceCID.
package car

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	commcid "github.com/filecoin-project/go-fil-commcid"
	commp "github.com/filecoin-project/go-fil-commp-hashhash"
	"github.com/ipfs/go-cid"
)

// MinBytes is the smallest CAR a PieceCID can be computed for.
const MinBytes = 65

// header is the CARv1 header as dag-cbor: {"roots": [roots...], "version": 1}.
func header(roots ...cid.Cid) []byte {
	h := []byte{0xa2, 0x65}
	h = append(h, "roots"...)
	if len(roots) < 24 {
		h = append(h, 0x80|byte(len(roots)))
	} else {
		h = append(h, 0x98, byte(len(roots)))
	}
	for _, root := range roots {
		cb := append([]byte{0}, root.Bytes()...) // tag 42 bytes start with the identity multibase prefix
		h = append(h, 0xd8, 0x2a)
		switch n := len(cb); {
		case n < 24:
			h = append(h, 0x40|byte(n))
		case n < 256:
			h = append(h, 0x58, byte(n))
		default:
			h = append(h, 0x59, byte(n>>8), byte(n))
		}
		h = append(h, cb...)
	}
	h = append(h, 0x67)
	h = append(h, "version"...)
	return append(h, 0x01)
}

func uvarintLen(n uint64) int64 {
	var buf [binary.MaxVarintLen64]byte
	return int64(binary.PutUvarint(buf[:], n))
}

// HeaderSize is the number of bytes the header naming roots takes.
func HeaderSize(roots ...cid.Cid) int64 {
	n := uint64(len(header(roots...)))
	return uvarintLen(n) + int64(n)
}

// EntrySize is the number of bytes one block with CID c and n bytes of data takes.
func EntrySize(c cid.Cid, n int) int64 {
	l := uint64(c.ByteLen() + n)
	return uvarintLen(l) + int64(l)
}

// Summary describes a written CAR.
type Summary struct {
	Bytes    int64
	Blocks   int64
	PieceCID cid.Cid // v2
}

// Writer streams a CARv1 to an io.Writer while computing its PieceCID.
type Writer struct {
	w      *bufio.Writer
	cp     *commp.Calc
	bytes  int64
	blocks int64
}

// MaxRoots is the most roots a header written here may name.
const MaxRoots = 255

// NewWriter writes the header naming roots (at least one, at most MaxRoots)
// and returns a Writer for the blocks.
func NewWriter(w io.Writer, roots ...cid.Cid) (*Writer, error) {
	if len(roots) < 1 || len(roots) > MaxRoots {
		return nil, fmt.Errorf("a CAR header here names 1 to %d roots, not %d", MaxRoots, len(roots))
	}
	cw := &Writer{cp: &commp.Calc{}}
	cw.w = bufio.NewWriterSize(io.MultiWriter(w, cw.cp), 1<<20)
	return cw, cw.section(header(roots...))
}

func (w *Writer) section(parts ...[]byte) error {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	var buf [binary.MaxVarintLen64]byte
	k, err := w.w.Write(buf[:binary.PutUvarint(buf[:], uint64(n))])
	w.bytes += int64(k)
	if err != nil {
		return err
	}
	for _, p := range parts {
		k, err := w.w.Write(p)
		w.bytes += int64(k)
		if err != nil {
			return err
		}
	}
	return nil
}

// Put appends one block. The caller has checked data against c.
func (w *Writer) Put(c cid.Cid, data []byte) error {
	w.blocks++
	return w.section(c.Bytes(), data)
}

// Finish flushes the CAR and returns its size and PieceCID.
func (w *Writer) Finish() (Summary, error) {
	if err := w.w.Flush(); err != nil {
		return Summary{}, err
	}
	if w.bytes < MinBytes {
		return Summary{}, fmt.Errorf("CAR is %d bytes; a piece needs at least %d", w.bytes, MinBytes)
	}
	raw, _, err := w.cp.Digest()
	if err != nil {
		return Summary{}, err
	}
	pc, err := commcid.DataCommitmentToPieceCidv2(raw, uint64(w.bytes))
	if err != nil {
		return Summary{}, err
	}
	return Summary{Bytes: w.bytes, Blocks: w.blocks, PieceCID: pc}, nil
}

// PieceCID computes the v2 PieceCID of everything r yields.
func PieceCID(r io.Reader) (cid.Cid, int64, error) {
	cp := &commp.Calc{}
	n, err := io.Copy(cp, r)
	if err != nil {
		return cid.Undef, 0, err
	}
	raw, _, err := cp.Digest()
	if err != nil {
		return cid.Undef, 0, err
	}
	pc, err := commcid.DataCommitmentToPieceCidv2(raw, uint64(n))
	return pc, n, err
}

// PayloadSize is the CAR bytes a v2 PieceCID says its piece holds, or 0 if
// pieceCID is not a v2 PieceCID.
func PayloadSize(pieceCID string) int64 {
	c, err := cid.Decode(pieceCID)
	if err != nil {
		return 0
	}
	_, n, err := commcid.PieceCidV2ToDataCommitment(c)
	if err != nil {
		return 0
	}
	return int64(n)
}

// Reader reads a CARv1 written by Writer.
type Reader struct {
	r     *bufio.Reader
	Root  cid.Cid   // the first root
	Roots []cid.Cid // every root, Root first
}

var ErrBadCAR = errors.New("not a CARv1 file")

func NewReader(r io.Reader) (*Reader, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	h, err := readSection(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCAR, err)
	}
	roots, err := parseHeader(h)
	if err != nil {
		return nil, err
	}
	return &Reader{r: br, Root: roots[0], Roots: roots}, nil
}

// parseHeader accepts exactly the header shapes Writer produces.
func parseHeader(h []byte) ([]cid.Cid, error) {
	pre := append([]byte{0xa2, 0x65}, "roots"...)
	if len(h) < len(pre)+2 || string(h[:len(pre)]) != string(pre) {
		return nil, fmt.Errorf("%w: header is not {roots: [cid...], version: 1}", ErrBadCAR)
	}
	var count int
	switch a := h[len(pre)]; {
	case a >= 0x81 && a <= 0x97:
		count, h = int(a&0x1f), h[len(pre)+1:]
	case a == 0x98 && h[len(pre)+1] >= 24:
		count, h = int(h[len(pre)+1]), h[len(pre)+2:]
	default:
		return nil, fmt.Errorf("%w: header is not {roots: [cid...], version: 1}", ErrBadCAR)
	}
	var roots []cid.Cid
	for range count {
		if len(h) < 4 || h[0] != 0xd8 || h[1] != 0x2a {
			return nil, fmt.Errorf("%w: bad root encoding", ErrBadCAR)
		}
		h = h[2:]
		var n int
		switch {
		case h[0]&0xe0 == 0x40 && h[0] < 0x58:
			n, h = int(h[0]&0x1f), h[1:]
		case h[0] == 0x58:
			n, h = int(h[1]), h[2:]
		case h[0] == 0x59 && len(h) > 2:
			n, h = int(h[1])<<8|int(h[2]), h[3:]
		default:
			return nil, fmt.Errorf("%w: bad root encoding", ErrBadCAR)
		}
		if n < 1 || len(h) < n || h[0] != 0 {
			return nil, fmt.Errorf("%w: bad root", ErrBadCAR)
		}
		c, err := cid.Cast(h[1:n])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadCAR, err)
		}
		roots, h = append(roots, c), h[n:]
	}
	if tail := append([]byte{0x67}, "version\x01"...); string(h) != string(tail) {
		return nil, fmt.Errorf("%w: header is not version 1", ErrBadCAR)
	}
	return roots, nil
}

// Next returns the next block, or io.EOF after the last one. It does not
// check the block's hash.
func (r *Reader) Next() (cid.Cid, []byte, error) {
	s, err := readSection(r.r)
	if err != nil {
		return cid.Undef, nil, err
	}
	n, c, err := cid.CidFromBytes(s)
	if err != nil {
		return cid.Undef, nil, fmt.Errorf("%w: %v", ErrBadCAR, err)
	}
	return c, s[n:], nil
}

const maxSection = 64 << 20

func readSection(r *bufio.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("%w: %v", ErrBadCAR, err)
	}
	if n == 0 || n > maxSection {
		return nil, fmt.Errorf("%w: section of %d bytes", ErrBadCAR, n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("%w: truncated: %v", ErrBadCAR, err)
	}
	return b, nil
}
