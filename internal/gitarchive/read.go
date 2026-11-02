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

// Blocks is an in-memory block set, checked against CIDs as it is filled.
type Blocks struct {
	m     map[string][]byte
	roots []cid.Cid
}

func NewBlocks() *Blocks { return &Blocks{m: map[string][]byte{}} }

// Put checks data against c and keeps it.
func (b *Blocks) Put(c cid.Cid, data []byte) error {
	got, err := c.Prefix().Sum(data)
	if err != nil {
		return err
	}
	if !got.Equals(c) {
		return fmt.Errorf("block %s: bytes hash to %s", c, got)
	}
	b.m[string(c.Hash())] = data
	return nil
}

// Get returns a block or an error naming it.
func (b *Blocks) Get(c cid.Cid) ([]byte, error) {
	if d, ok := b.m[string(c.Hash())]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("block %s is not in the archive", c)
}

// Roots are the CAR header roots seen while loading.
func (b *Blocks) Roots() []cid.Cid { return b.roots }

// Load reads a CAR file, or every .car file in a directory, into b.
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
	defer f.Close()
	r, err := car.NewReader(f)
	if err != nil {
		return err
	}
	b.roots = append(b.roots, r.Roots...)
	for {
		c, data, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := b.Put(c, data); err != nil {
			return err
		}
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
	var recs []cid.Cid
	prev := map[string]bool{}
	for _, c := range b.roots {
		r, err := LoadRecord(b.Get, c)
		if err != nil {
			continue
		}
		recs = append(recs, c)
		if r.Prev != nil {
			prev[r.Prev.KeyString()] = true
		}
	}
	var heads []cid.Cid
	seen := map[string]bool{}
	for _, c := range recs {
		if !prev[c.KeyString()] && !seen[c.KeyString()] {
			heads = append(heads, c)
			seen[c.KeyString()] = true
		}
	}
	switch len(heads) {
	case 0:
		return cid.Undef, errors.New("no git archive record among the CAR roots")
	case 1:
		return heads[0], nil
	}
	return cid.Undef, fmt.Errorf("%d unrelated snapshots; name one with --root", len(heads))
}
