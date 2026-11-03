package repo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ipfs/go-cid"
	carv2 "github.com/ipld/go-car/v2"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/gitarchive"
)

// carRepo reads one CAR file, or every .car file under a directory, as a
// repo. Each CAR's header roots are recursive pins named after the file, the
// way 'ipfs dag import' pins them. Blocks are indexed by file offset once, at
// open, and read from the files on demand.
type carRepo struct {
	path  string
	names []string // the CAR files, by index
	pins  []Pin
	order []string            // multihashes in first-seen order
	at    map[string]carBlock // multihash -> where its bytes are

	// Files open on demand, at most maxOpenCARs at a time, so a folder of
	// thousands of CARs does not run out of file descriptors.
	mu   sync.RWMutex
	open map[int]*os.File
	lru  []int // open files, oldest first
}

// maxOpenCARs bounds the CAR files a carRepo keeps open.
var maxOpenCARs = 64

type carBlock struct {
	file int
	off  int64
	size int
}

// isCAR says whether path is a CAR file, or a directory holding .car files
// and no fs-repo.
func isCAR(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return carFile(path)
	}
	if _, err := os.Stat(filepath.Join(path, "config")); err == nil {
		return false
	}
	files, _ := carFiles(path)
	return len(files) > 0
}

// carFile says whether the file at path starts like a CAR: a varint length,
// then a dag-cbor map holding "roots" or "version".
func carFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if l, k := binary.Uvarint(head); l == 0 || k <= 0 {
		return false
	}
	return bytes.Contains(head, []byte("version")) || bytes.Contains(head, []byte("roots"))
}

// carFiles lists the .car files under dir, sorted, so pins come in a fixed order.
func carFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != dir && gitarchive.IsGitRepo(p) {
			return fs.SkipDir // a git repo's own files; a folder of repos is not a folder of CARs
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".car") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func openCAR(path string) (Repo, error) {
	names := []string{path}
	base := filepath.Dir(path)
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.IsDir() {
		if names, err = carFiles(path); err != nil {
			return nil, err
		}
		base = path
	}
	r := &carRepo{path: path, at: map[string]carBlock{}}
	for _, name := range names {
		if err := r.index(name, base); err != nil {
			r.Close()
			return nil, err
		}
	}
	return r, nil
}

// index reads one CAR's header and block offsets.
func (r *carRepo) index(name, base string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	r.names = append(r.names, name)
	br, err := carv2.NewBlockReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	pin := strings.TrimSuffix(filepath.ToSlash(mustRel(base, name)), filepath.Ext(name))
	for _, c := range br.Roots {
		r.pins = append(r.pins, Pin{Root: c, Mode: Recursive, Name: pin})
	}
	for {
		m, err := br.SkipNext()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		k := string(m.Cid.Hash())
		if _, ok := r.at[k]; ok {
			continue
		}
		// SourceOffset is where the section starts: a varint length, the CID,
		// then the block.
		cidLen := uint64(m.Cid.ByteLen())
		var buf [binary.MaxVarintLen64]byte
		lenLen := uint64(binary.PutUvarint(buf[:], cidLen+m.Size))
		r.order = append(r.order, k)
		r.at[k] = carBlock{file: len(r.names) - 1, off: int64(m.SourceOffset + lenLen + cidLen), size: int(m.Size)}
	}
}

func mustRel(base, name string) string {
	if rel, err := filepath.Rel(base, name); err == nil {
		return rel
	}
	return filepath.Base(name)
}

func (r *carRepo) Path() string     { return r.path }
func (r *carRepo) Kind() string     { return KindCAR }
func (r *carRepo) Version() Version { return 0 }

func (r *carRepo) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []error
	for _, f := range r.open {
		errs = append(errs, f.Close())
	}
	r.open, r.lru = nil, nil
	return errors.Join(errs...)
}

// readAt reads from CAR file i, opening it if it is not open.
func (r *carRepo) readAt(i int, data []byte, off int64) error {
	for {
		r.mu.RLock()
		f := r.open[i]
		if f != nil {
			_, err := f.ReadAt(data, off)
			r.mu.RUnlock()
			return err
		}
		r.mu.RUnlock()
		r.mu.Lock()
		if r.open[i] == nil {
			nf, err := os.Open(r.names[i])
			if err != nil {
				r.mu.Unlock()
				return err
			}
			if r.open == nil {
				r.open = map[int]*os.File{}
			}
			for len(r.lru) >= maxOpenCARs {
				r.open[r.lru[0]].Close()
				delete(r.open, r.lru[0])
				r.lru = r.lru[1:]
			}
			r.open[i] = nf
			r.lru = append(r.lru, i)
		}
		r.mu.Unlock()
	}
}

func (r *carRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	for _, k := range r.order {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(multihash.Multihash(k), r.at[k].size); err != nil {
			return err
		}
	}
	return nil
}

func (r *carRepo) Get(_ context.Context, mh multihash.Multihash) ([]byte, error) {
	b, ok := r.at[string(mh)]
	if !ok {
		return nil, ErrNotFound
	}
	data := make([]byte, b.size)
	if err := r.readAt(b.file, data, b.off); err != nil {
		return nil, err
	}
	return data, nil
}

func (r *carRepo) Pins(context.Context) ([]Pin, error) { return r.pins, nil }

func (r *carRepo) MFSRoot(context.Context) (cid.Cid, bool, error) { return cid.Undef, false, nil }
