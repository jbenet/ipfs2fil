package repo

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// b32 is the datastore key encoding for blocks from repo 4 on.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// keyCodecs are the CID codecs tried, in order, to find a block by multihash in
// repos 5..11, which key blocks by CID. Anything else falls back to an index.
var keyCodecs = []uint64{cid.Raw, cid.DagProtobuf, cid.DagCBOR, cid.Libp2pKey, cid.DagJSON, cid.GitRaw}

type fsRepo struct {
	path string
	v    Version
	ds   kv
	// blockPrefix is where blocks live in the datastore: "/b/" in leveldb
	// for repos 0 and 1, "/blocks/" later.
	blockPrefix string

	indexOnce sync.Once
	index     map[string]string // multihash -> datastore key, for misses
	indexErr  error
}

func openFS(path string, v Version) (Repo, error) {
	ds, err := openDatastore(path, v)
	if err != nil {
		return nil, err
	}
	r := &fsRepo{path: path, v: v, ds: ds, blockPrefix: "/blocks/"}
	if v <= 1 {
		r.blockPrefix = "/b/"
		// Some early repos used the /blocks prefix in leveldb too.
		found := errors.New("found")
		if ds.Query("/b/", func(string, int) error { return found }) == nil {
			r.blockPrefix = "/blocks/"
		}
	}
	return r, nil
}

func (r *fsRepo) Path() string     { return r.path }
func (r *fsRepo) Kind() string     { return KindFSRepo }
func (r *fsRepo) Version() Version { return r.v }
func (r *fsRepo) Close() error     { return r.ds.Close() }

// keyHash turns a block's datastore key into its multihash. ok is false when
// the key does not decode; the caller then hashes the block itself.
func (r *fsRepo) keyHash(key string) (multihash.Multihash, bool) {
	name := key[len(r.blockPrefix):]
	var b []byte
	if r.v <= 3 {
		b = []byte(name) // raw multihash bytes
	} else {
		var err error
		if b, err = b32.DecodeString(name); err != nil {
			return nil, false
		}
	}
	if r.v >= 5 && r.v <= 11 {
		if c, err := cid.Cast(b); err == nil {
			return c.Hash(), true
		}
	}
	if m, err := multihash.Cast(b); err == nil {
		return m, true
	}
	if c, err := cid.Cast(b); err == nil {
		return c.Hash(), true
	}
	return nil, false
}

func (r *fsRepo) ForEachBlock(ctx context.Context, fn func(mh multihash.Multihash, size int) error) error {
	return r.forEachKey(ctx, func(key string, mh multihash.Multihash, size int) error {
		return fn(mh, size)
	})
}

func (r *fsRepo) forEachKey(ctx context.Context, fn func(key string, mh multihash.Multihash, size int) error) error {
	return r.ds.Query(r.blockPrefix, func(key string, size int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mh, ok := r.keyHash(key)
		if !ok {
			// Before repo 4 keys were raw bytes run through path.Clean,
			// which can mangle a multihash holding '/'. Those blocks were
			// all sha2-256, so hash the block to recover its key.
			if r.v > 3 {
				return fmt.Errorf("block key %q is neither a CID nor a multihash", key)
			}
			data, err := r.ds.Get(key)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			mh, _ = multihash.Encode(sum[:], multihash.SHA2_256)
		}
		return fn(key, mh, size)
	})
}

func (r *fsRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	dm, err := multihash.Decode(mh)
	if err != nil {
		return nil, fmt.Errorf("bad multihash: %w", err)
	}
	if dm.Code == multihash.IDENTITY {
		return dm.Digest, nil
	}
	for _, k := range r.candidateKeys(mh) {
		b, err := r.ds.Get(k)
		if err == nil {
			return b, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if r.v >= 12 {
		return nil, ErrNotFound // keys are exactly the multihash
	}
	r.indexOnce.Do(func() { r.indexErr = r.buildIndex(ctx) })
	if r.indexErr != nil {
		return nil, r.indexErr
	}
	k, ok := r.index[string(mh)]
	if !ok {
		return nil, ErrNotFound
	}
	return r.ds.Get(k)
}

// candidateKeys lists the datastore keys a block with this multihash would
// most likely have.
func (r *fsRepo) candidateKeys(mh multihash.Multihash) []string {
	switch {
	case r.v <= 3:
		return []string{r.blockPrefix + string(mh)}
	case r.v == 4 || r.v >= 12:
		return []string{r.blockPrefix + b32.EncodeToString(mh)}
	}
	ks := make([]string, 0, len(keyCodecs)+1)
	if c, err := cid.Cast(mh); err == nil && c.Version() == 0 {
		ks = append(ks, r.blockPrefix+b32.EncodeToString(mh)) // CIDv0
	}
	for _, codec := range keyCodecs {
		ks = append(ks, r.blockPrefix+b32.EncodeToString(cid.NewCidV1(codec, mh).Bytes()))
	}
	return ks
}

// buildIndex maps every block's multihash to its key, for blocks that
// candidateKeys does not find (rare codecs, mangled pre-repo-4 keys).
func (r *fsRepo) buildIndex(ctx context.Context) error {
	r.index = map[string]string{}
	return r.forEachKey(ctx, func(key string, mh multihash.Multihash, _ int) error {
		r.index[string(mh)] = key
		return nil
	})
}
