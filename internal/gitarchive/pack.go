package gitarchive

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"sort"
)

// PackObject is one object in a pack: its id, where its entry starts, and the
// CRC32 of the entry's raw bytes, which is what a git .idx records.
type PackObject struct {
	ID     []byte
	Offset int64
	CRC    uint32
	Type   int // resolved type: commit, tree, blob or tag
	Size   int64
}

// PackIndex is what IndexPack learns about a pack.
type PackIndex struct {
	Format   Format
	Objects  []PackObject // in pack order
	Checksum []byte       // the pack's trailer
	Thin     bool         // some deltas use bases outside the pack
}

// ExternalBase returns an object a thin pack's delta is based on but does
// not hold, read from the repo the pack was made from.
type ExternalBase func(id []byte) (typ int, data []byte, err error)

// Visit sees every object's full contents once, after its id is known. Data
// is nil for blobs and trees, which callers here do not need; it is only
// valid during the call.
type Visit func(o PackObject, data []byte) error

// maxObject caps what IndexPack will inflate into memory for one object. git
// has no limit; this one is a sanity bound against a corrupt size header.
const maxObject = 4 << 30

// entry is a pack entry as stored: possibly a delta.
type entry struct {
	off      int64
	dataOff  int64 // where the zlib stream starts
	end      int64
	typ      int   // stored type, may be a delta
	size     int64 // inflated size of what is stored (the delta, for deltas)
	baseOff  int64 // ofs-delta base
	baseID   string
	id       []byte
	realType int
	realSize int64
	crc      uint32
}

// countingReader reads one byte at a time from a bufio.Reader so zlib never
// reads past the end of its stream, and keeps the raw bytes for the CRC.
type countingReader struct {
	r   *bufio.Reader
	n   int64
	raw []byte
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
		c.raw = append(c.raw, b)
	}
	return b, err
}

func (c *countingReader) Read(p []byte) (int, error) {
	// flate only uses ReadByte when it is offered; Read must still not
	// over-read, so hand out one byte at a time.
	if len(p) == 0 {
		return 0, nil
	}
	b, err := c.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = b
	return 1, nil
}

// IndexPack reads a pack of size bytes from r, works out every object's id
// (resolving deltas, with external for a thin pack's outside bases), checks
// SHA-1DC and the pack trailer, and calls visit for each object.
func IndexPack(r io.ReaderAt, size int64, f Format, external ExternalBase, visit Visit) (*PackIndex, error) {
	if size < 12+int64(f.Size) {
		return nil, errors.New("pack too short")
	}
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, 0); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != "PACK" {
		return nil, errors.New("not a git pack")
	}
	if v := binary.BigEndian.Uint32(hdr[4:]); v != 2 && v != 3 {
		return nil, fmt.Errorf("pack version %d not supported", v)
	}
	count := binary.BigEndian.Uint32(hdr[8:])
	body := size - int64(f.Size)

	// The trailer is the hash of everything before it.
	h := f.hasher()
	if _, err := io.Copy(h, io.NewSectionReader(r, 0, body)); err != nil {
		return nil, err
	}
	want := make([]byte, f.Size)
	if _, err := r.ReadAt(want, body); err != nil {
		return nil, err
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return nil, fmt.Errorf("pack checksum mismatch: trailer %x, contents hash to %x", want, got)
	}

	ix := &packIndexer{r: r, f: f, visit: visit, byOff: map[int64]int{}, ofsKids: map[int64][]int{}, refKids: map[string][]int{}}
	if count > uint32(body/2) {
		return nil, fmt.Errorf("pack claims %d objects in %d bytes", count, body)
	}
	ix.entries = make([]entry, 0, min(count, 1<<20))
	if err := ix.scan(body, count); err != nil {
		return nil, err
	}
	if err := ix.resolve(external); err != nil {
		return nil, err
	}
	out := &PackIndex{Format: f, Checksum: want, Thin: ix.thin}
	out.Objects = make([]PackObject, len(ix.entries))
	for i, e := range ix.entries {
		out.Objects[i] = PackObject{ID: e.id, Offset: e.off, CRC: e.crc, Type: e.realType, Size: e.realSize}
	}
	return out, nil
}

type packIndexer struct {
	r       io.ReaderAt
	f       Format
	visit   Visit
	entries []entry
	byOff   map[int64]int
	ofsKids map[int64][]int
	refKids map[string][]int
	thin    bool
	zr      io.ReadCloser
	buf     bytes.Buffer
}

// scan is pass one: walk every entry in order, inflating each to find where
// it ends. Whole objects are hashed now; deltas wait for pass two.
func (ix *packIndexer) scan(body int64, count uint32) error {
	br := bufio.NewReaderSize(io.NewSectionReader(ix.r, 12, body-12), 1<<20)
	cr := &countingReader{r: br, n: 12}
	for i := uint32(0); i < count; i++ {
		cr.raw = cr.raw[:0]
		e := entry{off: cr.n}
		c, err := cr.ReadByte()
		if err != nil {
			return fmt.Errorf("object %d: %w", i, unexpected(err))
		}
		e.typ = int(c>>4) & 7
		e.size = int64(c & 15)
		for shift := uint(4); c&0x80 != 0; shift += 7 {
			if shift > 56 {
				return fmt.Errorf("object at %d: size header too long", e.off)
			}
			if c, err = cr.ReadByte(); err != nil {
				return unexpected(err)
			}
			e.size |= int64(c&0x7f) << shift
		}
		switch e.typ {
		case objCommit, objTree, objBlob, objTag:
		case objOfsDelta:
			c, err := cr.ReadByte()
			if err != nil {
				return unexpected(err)
			}
			rel := int64(c & 0x7f)
			for c&0x80 != 0 {
				if rel > 1<<55 {
					return fmt.Errorf("object at %d: delta offset too long", e.off)
				}
				if c, err = cr.ReadByte(); err != nil {
					return unexpected(err)
				}
				rel = (rel+1)<<7 | int64(c&0x7f)
			}
			e.baseOff = e.off - rel
			if rel <= 0 || e.baseOff < 12 {
				return fmt.Errorf("object at %d: delta base offset %d is outside the pack", e.off, e.baseOff)
			}
		case objRefDelta:
			id := make([]byte, ix.f.Size)
			for j := range id {
				if id[j], err = cr.ReadByte(); err != nil {
					return unexpected(err)
				}
			}
			e.baseID = string(id)
		default:
			return fmt.Errorf("object at %d: bad type %d", e.off, e.typ)
		}
		if e.size > maxObject {
			return fmt.Errorf("object at %d: %d bytes is past the %d byte limit", e.off, e.size, int64(maxObject))
		}
		e.dataOff = cr.n
		whole := e.typ != objOfsDelta && e.typ != objRefDelta
		var h hash.Hash
		w := io.Discard
		keep := whole && (e.typ == objCommit || e.typ == objTag)
		ix.buf.Reset()
		if whole {
			h = ix.f.hasher()
			h.Write(objectHeader(e.typ, e.size))
			w = h
			if keep {
				w = io.MultiWriter(h, &ix.buf)
			}
		}
		if err := ix.inflate(cr, e.size, w); err != nil {
			return fmt.Errorf("object at %d: %w", e.off, err)
		}
		e.end = cr.n
		e.crc = crc32.ChecksumIEEE(cr.raw)
		idx := len(ix.entries)
		ix.byOff[e.off] = idx
		ix.entries = append(ix.entries, e)
		if whole {
			var data []byte
			if keep {
				data = ix.buf.Bytes()
			}
			if err := ix.whole(idx, h, data); err != nil {
				return err
			}
		} else if e.typ == objOfsDelta {
			ix.ofsKids[e.baseOff] = append(ix.ofsKids[e.baseOff], idx)
		} else {
			ix.refKids[e.baseID] = append(ix.refKids[e.baseID], idx)
		}
	}
	if cr.n != body {
		return fmt.Errorf("pack has %d bytes after its last object", body-cr.n)
	}
	for off := range ix.ofsKids {
		if _, ok := ix.byOff[off]; !ok {
			return fmt.Errorf("delta base at offset %d is not an object", off)
		}
	}
	return nil
}

// whole finishes a non-delta object hashed in pass one.
func (ix *packIndexer) whole(idx int, h hash.Hash, data []byte) error {
	e := &ix.entries[idx]
	e.realType, e.realSize = e.typ, e.size
	id, err := ix.f.sum(h)
	if err != nil {
		return err
	}
	e.id = id
	return ix.emit(idx, data)
}

func (ix *packIndexer) emit(idx int, data []byte) error {
	e := &ix.entries[idx]
	if ix.visit == nil {
		return nil
	}
	if e.realType != objCommit && e.realType != objTag {
		data = nil
	}
	return ix.visit(PackObject{ID: e.id, Offset: e.off, CRC: e.crc, Type: e.realType, Size: e.realSize}, data)
}

// inflate reads one zlib stream of want bytes from cr into w.
func (ix *packIndexer) inflate(cr *countingReader, want int64, w io.Writer) error {
	var err error
	if ix.zr == nil {
		ix.zr, err = zlib.NewReader(cr)
	} else {
		err = ix.zr.(zlib.Resetter).Reset(cr, nil)
	}
	if err != nil {
		return fmt.Errorf("zlib: %w", err)
	}
	n, err := io.Copy(w, io.LimitReader(ix.zr, want+1))
	if err != nil {
		return fmt.Errorf("zlib: %w", err)
	}
	if n != want {
		return fmt.Errorf("inflates to %d bytes, header says %d", n, want)
	}
	// Read to the end of the stream so the adler32 trailer is consumed and checked.
	var one [1]byte
	if k, err := ix.zr.Read(one[:]); k != 0 || err != io.EOF {
		if err == nil || err == io.EOF {
			err = errors.New("stream longer than its header says")
		}
		return fmt.Errorf("zlib: %w", err)
	}
	return nil
}

// inflateTo re-reads e's stored bytes into w.
func (ix *packIndexer) inflateTo(e *entry, w io.Writer) error {
	zr, err := zlib.NewReader(io.NewSectionReader(ix.r, e.dataOff, e.end-e.dataOff))
	if err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(zr, e.size+1))
	if err != nil {
		return err
	}
	if n != e.size {
		return fmt.Errorf("object at %d inflates to %d bytes, header says %d", e.off, n, e.size)
	}
	return nil
}

func (ix *packIndexer) stored(e *entry) ([]byte, error) {
	var b bytes.Buffer
	b.Grow(int(min(e.size, 64<<20)))
	if err := ix.inflateTo(e, &b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// resolve is pass two: starting from each whole object that something is
// based on, apply its deltas depth first. Each delta is inflated once and a
// chain holds only its own ancestors in memory, as git's index-pack does.
func (ix *packIndexer) resolve(external ExternalBase) error {
	done := map[string]bool{}
	for i := range ix.entries {
		e := &ix.entries[i]
		if e.id == nil || (len(ix.ofsKids[e.off]) == 0 && len(ix.refKids[string(e.id)]) == 0) {
			continue
		}
		if e.typ == objOfsDelta || e.typ == objRefDelta {
			continue // resolved as some chain's member
		}
		data, err := ix.stored(e)
		if err != nil {
			return err
		}
		if err := ix.children(e.off, string(e.id), e.realType, data, done); err != nil {
			return err
		}
	}
	// Bases outside the pack: a thin pack's.
	var missing []string
	for id := range ix.refKids {
		if !done[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		if done[id] {
			continue // became known while resolving another chain
		}
		if external == nil {
			return fmt.Errorf("delta base %x is not in the pack", id)
		}
		typ, data, err := external([]byte(id))
		if err != nil {
			return fmt.Errorf("delta base %x: %w", id, err)
		}
		if got, err := ix.f.objectID(typ, data); err != nil {
			return err
		} else if string(got) != id {
			return fmt.Errorf("delta base %x: repo returned object %x", id, got)
		}
		ix.thin = true
		if err := ix.children(-1, id, typ, data, done); err != nil {
			return err
		}
	}
	for i := range ix.entries {
		if ix.entries[i].id == nil {
			return fmt.Errorf("object at %d: delta chain never resolves (a cycle, or a base that is itself unresolved)", ix.entries[i].off)
		}
	}
	return nil
}

// children applies every delta based on the object at off (or with id) and
// recurses into theirs.
func (ix *packIndexer) children(off int64, id string, typ int, base []byte, done map[string]bool) error {
	var kids []int
	if off >= 0 {
		kids = append(kids, ix.ofsKids[off]...)
	}
	if !done[id] {
		kids = append(kids, ix.refKids[id]...)
		done[id] = true
	}
	for _, k := range kids {
		e := &ix.entries[k]
		if e.id != nil {
			continue
		}
		delta, err := ix.stored(e)
		if err != nil {
			return err
		}
		data, err := applyDelta(base, delta)
		if err != nil {
			return fmt.Errorf("object at %d: %w", e.off, err)
		}
		e.realType, e.realSize = typ, int64(len(data))
		if e.id, err = ix.f.objectID(typ, data); err != nil {
			return err
		}
		if err := ix.emit(k, data); err != nil {
			return err
		}
		if err := ix.children(e.off, string(e.id), typ, data, done); err != nil {
			return err
		}
	}
	return nil
}

// applyDelta builds an object from base and a git delta.
func applyDelta(base, delta []byte) ([]byte, error) {
	src, delta, err := deltaVarint(delta)
	if err != nil {
		return nil, err
	}
	if src != uint64(len(base)) {
		return nil, fmt.Errorf("delta expects a %d byte base, have %d", src, len(base))
	}
	dst, delta, err := deltaVarint(delta)
	if err != nil {
		return nil, err
	}
	if dst > maxObject {
		return nil, fmt.Errorf("delta result of %d bytes is too big", dst)
	}
	// dst comes from the delta itself; don't trust it for an allocation.
	out := make([]byte, 0, min(dst, uint64(len(base))+uint64(len(delta))*8, 64<<20))
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		switch {
		case op&0x80 != 0:
			var off, n uint64
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if len(delta) == 0 {
						return nil, errors.New("truncated delta copy")
					}
					off |= uint64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if len(delta) == 0 {
						return nil, errors.New("truncated delta copy")
					}
					n |= uint64(delta[0]) << (8 * i)
					delta = delta[1:]
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > uint64(len(base)) || off+n < off {
				return nil, fmt.Errorf("delta copies %d bytes at %d from a %d byte base", n, off, len(base))
			}
			if uint64(len(out))+n > dst {
				return nil, errors.New("delta writes past its result size")
			}
			out = append(out, base[off:off+n]...)
		case op != 0:
			n := int(op)
			if n > len(delta) {
				return nil, errors.New("truncated delta insert")
			}
			if uint64(len(out)+n) > dst {
				return nil, errors.New("delta writes past its result size")
			}
			out = append(out, delta[:n]...)
			delta = delta[n:]
		default:
			return nil, errors.New("delta has reserved opcode 0")
		}
	}
	if uint64(len(out)) != dst {
		return nil, fmt.Errorf("delta made %d bytes, says %d", len(out), dst)
	}
	return out, nil
}

func deltaVarint(b []byte) (uint64, []byte, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, b[i+1:], nil
		}
	}
	return 0, nil, errors.New("bad delta header")
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
