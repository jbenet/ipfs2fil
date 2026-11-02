package gitarchive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// WriteIdx writes ix as a git pack index, version 2, the layout `git
// index-pack` writes (sha256 repos use the same layout with 32-byte ids). Thin
// packs get one too: git cannot index a thin pack without completing it, so
// we write it ourselves.
func WriteIdx(w io.Writer, ix *PackIndex) error {
	objs := append([]PackObject(nil), ix.Objects...)
	sort.Slice(objs, func(i, j int) bool { return bytes.Compare(objs[i].ID, objs[j].ID) < 0 })
	for i := 1; i < len(objs); i++ {
		if bytes.Equal(objs[i].ID, objs[i-1].ID) {
			return fmt.Errorf("object %x is in the pack twice", objs[i].ID)
		}
	}
	h := ix.Format.hasher()
	out := io.MultiWriter(w, h)
	var b bytes.Buffer
	b.Write([]byte{0xff, 't', 'O', 'c', 0, 0, 0, 2})
	var fan [256]uint32
	for _, o := range objs {
		fan[o.ID[0]]++
	}
	var n uint32
	for i := range fan {
		n += fan[i]
		binary.Write(&b, binary.BigEndian, n)
	}
	for _, o := range objs {
		b.Write(o.ID)
	}
	for _, o := range objs {
		binary.Write(&b, binary.BigEndian, o.CRC)
	}
	var large []uint64
	for _, o := range objs {
		if o.Offset < 0x80000000 {
			binary.Write(&b, binary.BigEndian, uint32(o.Offset))
		} else {
			binary.Write(&b, binary.BigEndian, uint32(0x80000000|len(large)))
			large = append(large, uint64(o.Offset))
		}
	}
	for _, o := range large {
		binary.Write(&b, binary.BigEndian, o)
	}
	b.Write(ix.Checksum)
	if _, err := out.Write(b.Bytes()); err != nil {
		return err
	}
	_, err := w.Write(h.Sum(nil))
	return err
}

// Idx is a parsed pack index: sorted ids and their offsets in the pack.
type Idx struct {
	Format   Format
	IDs      [][]byte
	Offsets  []int64
	CRCs     []uint32
	Checksum []byte // the pack's trailer
}

// ReadIdx parses a version 2 pack index and checks its own checksum.
func ReadIdx(data []byte, f Format) (*Idx, error) {
	hs := f.Size
	if len(data) < 8+256*4+2*hs || !bytes.Equal(data[:8], []byte{0xff, 't', 'O', 'c', 0, 0, 0, 2}) {
		return nil, errors.New("not a version 2 pack index")
	}
	h := f.hasher()
	h.Write(data[:len(data)-hs])
	if !bytes.Equal(h.Sum(nil), data[len(data)-hs:]) {
		return nil, errors.New("pack index checksum mismatch")
	}
	n := int(binary.BigEndian.Uint32(data[8+255*4:]))
	need := 8 + 256*4 + n*(hs+8) + 2*hs
	if n < 0 || len(data) < need {
		return nil, errors.New("pack index truncated")
	}
	ix := &Idx{Format: f, IDs: make([][]byte, n), Offsets: make([]int64, n), CRCs: make([]uint32, n)}
	p := 8 + 256*4
	for i := 0; i < n; i++ {
		ix.IDs[i] = data[p+i*hs : p+(i+1)*hs]
		if i > 0 && bytes.Compare(ix.IDs[i-1], ix.IDs[i]) >= 0 {
			return nil, errors.New("pack index ids are not sorted")
		}
	}
	p += n * hs
	for i := 0; i < n; i++ {
		ix.CRCs[i] = binary.BigEndian.Uint32(data[p+4*i:])
	}
	p += 4 * n
	lp := p + 4*n
	nlarge := (len(data) - 2*hs - lp) / 8
	for i := 0; i < n; i++ {
		v := binary.BigEndian.Uint32(data[p+4*i:])
		if v&0x80000000 == 0 {
			ix.Offsets[i] = int64(v)
			continue
		}
		k := int(v &^ 0x80000000)
		if k >= nlarge {
			return nil, errors.New("pack index large offset out of range")
		}
		ix.Offsets[i] = int64(binary.BigEndian.Uint64(data[lp+8*k:]))
	}
	ix.Checksum = data[len(data)-2*hs : len(data)-hs]
	return ix, nil
}

// Find returns the pack offset of id.
func (ix *Idx) Find(id []byte) (int64, bool) {
	i := sort.Search(len(ix.IDs), func(i int) bool { return bytes.Compare(ix.IDs[i], id) >= 0 })
	if i < len(ix.IDs) && bytes.Equal(ix.IDs[i], id) {
		return ix.Offsets[i], true
	}
	return 0, false
}
