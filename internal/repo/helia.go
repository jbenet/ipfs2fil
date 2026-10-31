package repo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// Helia has no repo of its own: an app hands it a blockstore and a
// datastore. ipfs2fil reads the ones Node apps use most, as @helia/utils,
// blockstore-fs, datastore-fs and @helia/mfs write them:
//
//   - blockstore-fs: <dir>/<last 2 chars>/<B + base32 multihash>.data
//   - the datastore, datastore-fs (<key>.data files) or datastore-level (a
//     leveldb), holding pins under /pin/<base36 CID> as cbor
//     {depth, metadata}, and the MFS root under /locals/filesroot (the
//     @helia/mfs default) or /local/filesroot.
//
// --repo names the blockstore directory, or a directory holding blocks/ or
// blockstore/; the datastore is datastore/ beside the blockstore. With no
// datastore every block is an orphan.
type heliaRepo struct {
	path   string
	blocks string // the blockstore directory
	ds     kv     // nil when there is no datastore
}

// heliaExt is blockstore-fs's file extension.
const heliaExt = ".data"

// heliaBlockstore finds the blockstore of a Helia store at path.
func heliaBlockstore(path string) (string, bool) {
	if _, err := os.Stat(filepath.Join(path, "config")); err == nil {
		return "", false // a kubo or js-ipfs repo
	}
	for _, dir := range []string{path, filepath.Join(path, "blockstore"), filepath.Join(path, "blocks")} {
		if isHeliaBlocks(dir) {
			return dir, true
		}
	}
	return "", false
}

// isHeliaBlocks looks for one block file named the way blockstore-fs names
// them: in a two-character shard directory matching its name's end, the name
// "B" plus base32 of a multihash.
func isHeliaBlocks(dir string) bool {
	shards, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, s := range shards {
		if !s.IsDir() || len(s.Name()) != 2 {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(dir, s.Name()))
		for _, f := range files {
			if _, ok := heliaHash(f.Name()); ok && strings.HasSuffix(strings.TrimSuffix(f.Name(), heliaExt), s.Name()) {
				return true
			}
		}
	}
	return false
}

// heliaHash reads a block file name.
func heliaHash(name string) (multihash.Multihash, bool) {
	s, ok := strings.CutSuffix(name, heliaExt)
	if !ok {
		return nil, false
	}
	s, ok = strings.CutPrefix(s, "B")
	if !ok {
		return nil, false
	}
	b, err := b32.DecodeString(s)
	if err != nil {
		return nil, false
	}
	mh, err := multihash.Cast(b)
	return mh, err == nil
}

func heliaName(mh multihash.Multihash) string {
	s := "B" + b32.EncodeToString(mh)
	return filepath.Join(s[len(s)-2:], s+heliaExt)
}

func openHelia(path, blocks string) (Repo, error) {
	r := &heliaRepo{path: path, blocks: blocks}
	ds := filepath.Join(filepath.Dir(blocks), "datastore")
	if st, err := os.Stat(ds); err == nil && st.IsDir() {
		if _, err := os.Stat(filepath.Join(ds, "CURRENT")); err == nil {
			l, err := openLevelDB(ds)
			if err != nil {
				return nil, err
			}
			r.ds = l
		} else {
			r.ds = fsDatastore(ds)
		}
	}
	return r, nil
}

func (r *heliaRepo) Path() string     { return r.path }
func (r *heliaRepo) Kind() string     { return KindHelia }
func (r *heliaRepo) Version() Version { return 0 }

func (r *heliaRepo) Close() error {
	if r.ds != nil {
		return r.ds.Close()
	}
	return nil
}

func (r *heliaRepo) Describe() string {
	if r.ds == nil {
		return "Helia blockstore, no datastore beside it"
	}
	return "Helia blockstore and datastore"
}

func (r *heliaRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	return filepath.WalkDir(r.blocks, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		mh, ok := heliaHash(d.Name())
		if !ok {
			return nil // a temp file, or not a block
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return fn(mh, int(fi.Size()))
	})
}

func (r *heliaRepo) Get(_ context.Context, mh multihash.Multihash) ([]byte, error) {
	if dm, err := multihash.Decode(mh); err == nil && dm.Code == multihash.IDENTITY {
		return dm.Digest, nil
	}
	b, err := os.ReadFile(filepath.Join(r.blocks, heliaName(mh)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (r *heliaRepo) Pins(context.Context) ([]Pin, error) {
	var ps []Pin
	if r.ds == nil {
		return ps, nil
	}
	var keys []string
	if err := r.ds.Query("/pin/", func(k string, _ int) error {
		keys = append(keys, k)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading pins: %w", err)
	}
	for _, k := range keys {
		c, err := cid.Decode(strings.TrimPrefix(k, "/pin/"))
		if err != nil {
			return nil, fmt.Errorf("pin key %q: %w", k, err)
		}
		b, err := r.ds.Get(k)
		if err != nil {
			return nil, err
		}
		var rec struct {
			Depth    float64        `cbor:"depth"`
			Metadata map[string]any `cbor:"metadata"`
		}
		if err := cbor.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("pin %s: %w", c, err)
		}
		// A pin of depth n > 0 holds n levels below the root; the archive
		// takes whatever of the DAG is here, as for a recursive pin.
		p := Pin{Root: c, Mode: Recursive}
		if rec.Depth == 0 {
			p.Mode = Direct
		} else if rec.Depth < 0 || (rec.Depth != math.Trunc(rec.Depth) && !math.IsInf(rec.Depth, 1)) {
			return nil, fmt.Errorf("pin %s has depth %v", c, rec.Depth)
		}
		if n, ok := rec.Metadata["name"].(string); ok {
			p.Name = n
		}
		ps = append(ps, p)
	}
	sortPins(ps)
	return ps, nil
}

func (r *heliaRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	if r.ds == nil {
		return cid.Undef, false, nil
	}
	for _, k := range []string{"/locals/filesroot", "/local/filesroot"} {
		b, err := r.ds.Get(k)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return cid.Undef, false, err
		}
		c, err := cid.Cast(b)
		if err != nil {
			return cid.Undef, false, fmt.Errorf("MFS root %s is not a CID: %w", k, err)
		}
		return c, true, nil
	}
	return cid.Undef, false, nil
}

// fsDatastore reads datastore-fs: each key is a file, <dir><key>.data.
type fsDatastore string

func (d fsDatastore) file(key string) string {
	return filepath.Join(string(d), filepath.FromSlash(strings.TrimPrefix(key, "/"))+heliaExt)
}

func (d fsDatastore) Get(key string) ([]byte, error) {
	b, err := os.ReadFile(d.file(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (d fsDatastore) Query(prefix string, fn func(string, int) error) error {
	// Walk only the directory the prefix names.
	start := filepath.Join(string(d), filepath.FromSlash(path.Dir(prefix+"x")))
	err := filepath.WalkDir(start, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(p, heliaExt) {
			return nil
		}
		rel, err := filepath.Rel(string(d), p)
		if err != nil {
			return err
		}
		key := "/" + strings.TrimSuffix(filepath.ToSlash(rel), heliaExt)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		return fn(key, int(fi.Size()))
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (d fsDatastore) Close() error { return nil }
