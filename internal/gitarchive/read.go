package gitarchive

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/car"
)

// Blocks is a block set, checked against CIDs: blocks Put are kept in
// memory, blocks in loaded CAR files stay on disk and are read (and checked
// again) when asked for, so restoring a big archive does not hold it all in
// memory.
type Blocks struct {
	m     map[string][]byte
	at    map[string]blockAt
	files []*os.File
	roots []cid.Cid
}

type blockAt struct {
	file int32
	n    int32
	off  int64
}

func NewBlocks() *Blocks { return &Blocks{m: map[string][]byte{}, at: map[string]blockAt{}} }

// Put checks data against c and keeps it.
func (b *Blocks) Put(c cid.Cid, data []byte) error {
	if err := check(c, data); err != nil {
		return err
	}
	b.m[string(c.Hash())] = data
	return nil
}

func check(c cid.Cid, data []byte) error {
	got, err := c.Prefix().Sum(data)
	if err != nil {
		return err
	}
	if !got.Equals(c) {
		return fmt.Errorf("block %s: bytes hash to %s", c, got)
	}
	return nil
}

// Get returns a block or an error naming it.
func (b *Blocks) Get(c cid.Cid) ([]byte, error) {
	k := string(c.Hash())
	if d, ok := b.m[k]; ok {
		return d, nil
	}
	at, ok := b.at[k]
	if !ok {
		return nil, fmt.Errorf("block %s is not in the archive", c)
	}
	d := make([]byte, at.n)
	if _, err := b.files[at.file].ReadAt(d, at.off); err != nil {
		return nil, fmt.Errorf("block %s: %w", c, err)
	}
	if err := check(c, d); err != nil {
		return nil, err // the file changed since it was loaded
	}
	return d, nil
}

// Has reports whether b holds a block.
func (b *Blocks) Has(c cid.Cid) bool {
	k := string(c.Hash())
	if _, ok := b.m[k]; ok {
		return true
	}
	_, ok := b.at[k]
	return ok
}

// Close closes the loaded CAR files.
func (b *Blocks) Close() error {
	for _, f := range b.files {
		f.Close()
	}
	b.files, b.at = nil, map[string]blockAt{}
	return nil
}

// Roots are the CAR header roots seen while loading.
func (b *Blocks) Roots() []cid.Cid { return b.roots }

// Load indexes a CAR file, or every .car file in a directory, checking every
// block's hash as it goes; the blocks stay in the files.
func (b *Blocks) Load(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	files := []string{path}
	if st.IsDir() {
		ents, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		files = files[:0]
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".car") {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
		sort.Strings(files)
		if len(files) == 0 {
			return fmt.Errorf("no .car files in %s", path)
		}
	}
	for _, f := range files {
		if err := b.loadFile(f); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

func (b *Blocks) loadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	r, err := car.NewReader(f)
	if err != nil {
		f.Close()
		return err
	}
	idx := int32(len(b.files))
	b.files = append(b.files, f)
	b.roots = append(b.roots, r.Roots...)
	off := car.HeaderSize(r.Roots...)
	for {
		c, data, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := check(c, data); err != nil {
			return err
		}
		n := car.EntrySize(c, len(data))
		if _, ok := b.at[string(c.Hash())]; !ok {
			b.at[string(c.Hash())] = blockAt{file: idx, n: int32(len(data)), off: off + n - int64(len(data))}
		}
		off += n
	}
}

// LoadRecord decodes the record at c.
func LoadRecord(get BlockGetter, c cid.Cid) (*Record, error) {
	if c.Type() != cid.DagCBOR {
		return nil, fmt.Errorf("%s is not a DAG-CBOR record", c)
	}
	data, err := get(c)
	if err != nil {
		return nil, err
	}
	return DecodeRecord(data)
}

// Snapshot is a record and its CID.
type Snapshot struct {
	Root   cid.Cid
	Record *Record
}

// maxChain bounds how many snapshots one archive may chain.
const maxChain = 1 << 16

// Chain follows Prev links from root and returns the snapshots oldest first.
func Chain(get BlockGetter, root cid.Cid) ([]Snapshot, error) {
	var out []Snapshot
	seen := map[string]bool{}
	for c := root; ; {
		if seen[c.KeyString()] {
			return nil, errors.New("snapshot chain loops")
		}
		if len(out) >= maxChain {
			return nil, fmt.Errorf("snapshot chain is longer than %d", maxChain)
		}
		seen[c.KeyString()] = true
		r, err := LoadRecord(get, c)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 && r.ObjectFormat != out[0].Record.ObjectFormat {
			return nil, errors.New("snapshot chain mixes object formats")
		}
		out = append(out, Snapshot{c, r})
		if r.Prev == nil {
			break
		}
		c = r.Prev.Cid
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Latest picks the newest snapshot among the CAR roots that are git
// archive records: the one no other record names as Prev.
func (b *Blocks) Latest() (cid.Cid, error) {
	heads, _ := LatestOf(b.Get, b.roots)
	switch len(heads) {
	case 0:
		return cid.Undef, errors.New("no git archive record among the CAR roots")
	case 1:
		return heads[0], nil
	}
	return cid.Undef, fmt.Errorf("%d unrelated snapshots; name one with --root", len(heads))
}
