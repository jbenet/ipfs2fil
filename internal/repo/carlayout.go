package repo

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"
)

// CARLayout is how one CAR file is laid out, beyond the blocks it holds: the
// bytes before its first block, the CID of every section in order (repeats
// included), and for a CARv2 file the bytes after its data, its index. With
// the blocks, that is all it takes to write the file again byte for byte.
type CARLayout struct {
	// Name is the file's path relative to the repo, with '/' separators: the
	// file's base name for a repo that is one CAR file.
	Name string
	// Prefix is every byte before the first section: the CARv1 header, or
	// for CARv2 the pragma, the v2 header, any padding and the inner header.
	Prefix []byte
	// Sections are the CIDs of the file's sections, in order, as written.
	Sections []cid.Cid
	// Suffix is every byte after the last section: a CARv2 index.
	Suffix []byte
	Size   int64
	SHA256 string // hex
}

// CARLayouter is a repo made of CAR files that can say how they are laid
// out, so an export can give back the very files it read.
type CARLayouter interface {
	CARLayouts() ([]CARLayout, error)
}

// CARLayouts reads the layout of each of the repo's CAR files.
func (r *carRepo) CARLayouts() ([]CARLayout, error) {
	base := filepath.Dir(r.path)
	if fi, err := os.Stat(r.path); err == nil && fi.IsDir() {
		base = r.path
	}
	var out []CARLayout
	for _, f := range r.files {
		l, err := ReadCARLayout(f.Name())
		if err != nil {
			return nil, err
		}
		l.Name = filepath.ToSlash(mustRel(base, f.Name()))
		out = append(out, *l)
	}
	return out, nil
}

// CARLayouts gives the layout of the account's repo CAR, named as it is in
// a fetched account's directory.
func (r *atprotoRepo) CARLayouts() ([]CARLayout, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	ls, err := r.car.CARLayouts()
	if err != nil {
		return nil, err
	}
	for i := range ls {
		ls[i].Name = "repo.car"
	}
	return ls, nil
}

// carV2Pragma starts every CARv2 file.
var carV2Pragma = []byte{0x0a, 0xa1, 0x67, 0x76, 0x65, 0x72, 0x73, 0x69, 0x6f, 0x6e, 0x02}

// ReadCARLayout reads the layout of the CAR file at path. Name is left
// empty.
func ReadCARLayout(path string) (*CARLayout, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	cr := &countingReader{r: bufio.NewReaderSize(io.TeeReader(f, h), 1<<20)}
	l := &CARLayout{Size: fi.Size()}

	// For CARv2, the sections run from the data offset for data size bytes.
	start, end := int64(0), fi.Size()
	var head [11 + 40]byte
	if _, err := io.ReadFull(cr, head[:11]); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	prefix := append([]byte(nil), head[:11]...)
	if string(head[:11]) == string(carV2Pragma) {
		if _, err := io.ReadFull(cr, head[11:]); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		start = int64(binary.LittleEndian.Uint64(head[11+16:]))
		size := int64(binary.LittleEndian.Uint64(head[11+24:]))
		end = start + size
		if start < int64(len(head)) || end > fi.Size() {
			return nil, fmt.Errorf("%s: bad CARv2 header", path)
		}
		pad := make([]byte, start-int64(len(head)))
		if _, err := io.ReadFull(cr, pad); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		prefix = append(append(prefix, head[11:]...), pad...)
		hb, err := readVarintSection(cr)
		if err != nil {
			return nil, fmt.Errorf("%s: inner header: %w", path, err)
		}
		prefix = append(prefix, hb...)
	} else {
		// A CARv1 file: the 11 bytes read are the start of its header.
		n, k := binary.Uvarint(head[:11])
		if k <= 0 {
			return nil, fmt.Errorf("%s: not a CAR file", path)
		}
		rest := int64(k) + int64(n) - 11
		if rest < 0 {
			return nil, fmt.Errorf("%s: header too short", path)
		}
		b := make([]byte, rest)
		if _, err := io.ReadFull(cr, b); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		prefix = append(prefix, b...)
	}
	l.Prefix = prefix

	for cr.n < end {
		n, err := binary.ReadUvarint(cr)
		if err != nil {
			return nil, fmt.Errorf("%s: section at %d: %w", path, cr.n, err)
		}
		if n == 0 {
			// A zero-length section, which some writers pad with, would
			// not round-trip through the blocks; count it as the end.
			return nil, fmt.Errorf("%s: empty section at %d", path, cr.n)
		}
		k, c, err := cid.CidFromReader(cr)
		if err != nil {
			return nil, fmt.Errorf("%s: section at %d: %w", path, cr.n, err)
		}
		if uint64(k) > n {
			return nil, fmt.Errorf("%s: section at %d is shorter than its CID", path, cr.n)
		}
		if _, err := io.CopyN(io.Discard, cr, int64(n)-int64(k)); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		l.Sections = append(l.Sections, c)
	}
	if cr.n != end {
		return nil, fmt.Errorf("%s: sections run past the data", path)
	}
	if end < fi.Size() {
		if l.Suffix, err = io.ReadAll(cr); err != nil {
			return nil, err
		}
	}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return nil, err
	}
	l.SHA256 = hex.EncodeToString(h.Sum(nil))
	return l, nil
}

// readVarintSection reads a varint length and that many bytes, returning
// both as read.
func readVarintSection(r *countingReader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	var vb [binary.MaxVarintLen64]byte
	k := binary.PutUvarint(vb[:], n)
	if n > 1<<24 {
		return nil, errors.New("too long")
	}
	b := make([]byte, k+int(n))
	copy(b, vb[:k])
	if _, err := io.ReadFull(r, b[k:]); err != nil {
		return nil, err
	}
	return b, nil
}

type countingReader struct {
	r *bufio.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}
