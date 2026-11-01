package archive

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Bundle gathers the small changes of every archive packed in one go into
// as few pieces as fit. Each piece added on chain costs about the same
// whatever its size, and each step adding pieces costs again, so an edit to
// several repos is one piece, not three or so per repo.
//
// Build hands the bundle, per archive: the head blocks (the root, its
// record, and what reads the manifest), the index and meta blocks no
// stored piece holds, and the last data piece if it is small. Build leaves
// the bundle's PieceCID empty in the manifest it writes; Finish writes the
// bundle CARs and fills them in. Until then the archive is not complete,
// and a pack run again rebuilds it.
//
// A bundle CAR's header names each archive's root and record in turn, and
// the records are its first blocks, so a catalog reader finds every archive
// in it from its first bytes. A block two archives both hand it is stored
// once.
type Bundle struct {
	MaxPiece int64
	// Archives counts the archives in the bundle pieces so far.
	Archives int
	cur      *openBundle
	pieces   []Piece // finished, as in the first member's pieces/
}

// NewBundle makes a bundle whose pieces are at most maxPiece bytes, as
// Options.MaxPiece.
func NewBundle(maxPiece int64) *Bundle {
	return &Bundle{MaxPiece: maxPiece}
}

type openBundle struct {
	dir     string // the first member's pieces/, where the files go
	body    *os.File
	bw      *bufio.Writer
	roots   []cid.Cid
	records [][]byte // in header order
	recSize int64
	size    int64 // entry bytes in body
	blocks  int64
	seen    map[string]bool
	members []string // the members' outDirs
	noRec   bool     // the members have roots but no records
}

// contribution is one archive's part of a bundle.
type contribution struct {
	outDir string
	root   cid.Cid // undefined: no head blocks
	record []byte
	size   int64 // CAR entry bytes, at most
	put    func(put func(c cid.Cid, data []byte) error) error
}

func (b *Bundle) header(ob *openBundle, c *contribution) int64 {
	roots := ob.roots
	if c.root.Defined() {
		roots = append(roots[:len(roots):len(roots)], c.root, c.root) // a record CID is the same length
	}
	if len(roots) == 0 {
		return 128 // names the first block
	}
	return car.HeaderSize(roots...)
}

// fits says whether c fits in the bundle: by size, by header, and by kind
// (a header lists root and record pairs, or roots alone).
func (b *Bundle) fits(ob *openBundle, c *contribution) bool {
	if c.root.Defined() && len(ob.roots)+2 > car.MaxRoots {
		return false
	}
	if c.root.Defined() && len(ob.roots) > 0 && ob.noRec != (c.record == nil) {
		return false
	}
	return b.header(ob, c)+ob.recSize+ob.size+c.size <= b.MaxPiece
}

// add puts one archive's part in the bundle, closing the open bundle first
// if it does not fit there.
func (b *Bundle) add(c *contribution) error {
	if b.cur != nil && !b.fits(b.cur, c) {
		if err := b.close(); err != nil {
			return err
		}
	}
	if b.cur == nil {
		dir := filepath.Join(c.outDir, "pieces")
		f, err := os.CreateTemp(dir, ".bundle-*")
		if err != nil {
			return err
		}
		b.cur = &openBundle{dir: dir, body: f, bw: bufio.NewWriterSize(f, 1<<20), seen: map[string]bool{}}
		if !b.fits(b.cur, c) {
			return fmt.Errorf("%d bytes do not fit in one piece", c.size)
		}
	}
	ob := b.cur
	nroots, nrecs, nrec := len(ob.roots), len(ob.records), ob.recSize
	if c.root.Defined() {
		ob.roots = append(ob.roots, c.root)
		ob.noRec = c.record == nil
		if c.record != nil {
			rc, err := RecordCID(c.record)
			if err != nil {
				return err
			}
			ob.roots = append(ob.roots, rc)
			ob.records = append(ob.records, c.record)
			ob.recSize += car.EntrySize(rc, len(c.record))
			ob.seen[string(rc.Hash())] = true
		}
	}
	var buf []byte
	err := c.put(func(k cid.Cid, data []byte) error {
		if ob.seen[string(k.Hash())] {
			return nil
		}
		ob.seen[string(k.Hash())] = true
		cb := k.Bytes()
		buf = binary.AppendUvarint(buf[:0], uint64(len(cb)+len(data)))
		buf = append(buf, cb...)
		if _, err := ob.bw.Write(buf); err != nil {
			return err
		}
		if _, err := ob.bw.Write(data); err != nil {
			return err
		}
		ob.size += car.EntrySize(k, len(data))
		ob.blocks++
		return nil
	})
	if err != nil {
		// The blocks written stay, unnamed; the archive is left out.
		ob.roots, ob.records, ob.recSize = ob.roots[:nroots], ob.records[:nrecs], nrec
		return err
	}
	ob.members = append(ob.members, c.outDir)
	b.Archives++
	return nil
}

// close writes the open bundle's CAR and fills it into its members.
func (b *Bundle) close() error {
	ob := b.cur
	b.cur = nil
	defer os.Remove(ob.body.Name())
	defer ob.body.Close()
	if err := ob.bw.Flush(); err != nil {
		return err
	}
	if _, err := ob.body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReaderSize(ob.body, 1<<20)
	next := func() (cid.Cid, []byte, error) {
		n, err := binary.ReadUvarint(br)
		if err != nil {
			return cid.Undef, nil, err
		}
		sec := make([]byte, n)
		if _, err := io.ReadFull(br, sec); err != nil {
			return cid.Undef, nil, err
		}
		k, c, err := cid.CidFromBytes(sec)
		if err != nil {
			return cid.Undef, nil, err
		}
		return c, sec[k:], nil
	}
	if len(ob.members) == 0 {
		return nil
	}
	roots := ob.roots
	if len(roots) == 0 {
		// Only data: the header names the first block, as a data piece's.
		c, _, err := next()
		if err != nil {
			return fmt.Errorf("an empty bundle: %w", err)
		}
		roots = []cid.Cid{c}
		if _, err := ob.body.Seek(0, io.SeekStart); err != nil {
			return err
		}
		br.Reset(ob.body)
	}
	path, sum, err := writePiece(ob.dir, roots, func(w *car.Writer) error {
		for i, rec := range ob.records {
			if err := w.Put(roots[2*i+1], rec); err != nil {
				return err
			}
		}
		for {
			c, data, err := next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading the bundle back: %w", err)
			}
			if err := w.Put(c, data); err != nil {
				return err
			}
		}
	})
	if err != nil {
		return err
	}
	p := manifest.Piece{PieceCID: sum.PieceCID.String(), CARBytes: sum.Bytes, Blocks: sum.Blocks, Part: manifest.PartBundle}
	b.pieces = append(b.pieces, Piece{Piece: p, Path: path})
	for _, out := range ob.members {
		if err := fillBundle(out, path, p); err != nil {
			return fmt.Errorf("%s: %w", out, err)
		}
	}
	return nil
}

// fillBundle links the bundle CAR at path into the archive in outDir and
// sets its PieceCID in the archive's manifest.
func fillBundle(outDir, path string, p manifest.Piece) error {
	to := filepath.Join(outDir, "pieces", p.PieceCID+".car")
	if to != path {
		os.Remove(to)
		if err := os.Link(path, to); err != nil {
			if err := copyFile(path, to); err != nil {
				return err
			}
		}
	}
	mp := filepath.Join(outDir, ManifestFile)
	m, err := manifest.Read(mp)
	if err != nil {
		return err
	}
	n := 0
	for i := range m.Pieces {
		if q := &m.Pieces[i]; q.Part == manifest.PartBundle && q.PieceCID == "" {
			q.PieceCID, q.CARBytes, q.Blocks = p.PieceCID, p.CARBytes, p.Blocks
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("the manifest has %d bundle pieces to fill in, not 1", n)
	}
	return m.Write(mp)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, to)
}

// Finish writes the open bundle, if any, fills it into its archives'
// manifests, and returns every bundle piece written. Call it after the last
// Build that used the bundle, whether or not that Build failed.
func (b *Bundle) Finish() ([]Piece, error) {
	if b.cur != nil {
		if err := b.close(); err != nil {
			return b.pieces, err
		}
	}
	return b.pieces, nil
}

// Discard drops the open bundle without writing it, for a run that stops.
// Its archives stay incomplete, and pack rebuilds them.
func (b *Bundle) Discard() {
	if b.cur != nil {
		b.cur.body.Close()
		os.Remove(b.cur.body.Name())
		b.cur = nil
	}
}
