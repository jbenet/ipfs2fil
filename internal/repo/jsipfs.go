package repo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// js-ipfs repos (~/.jsipfs), versions 7 through 12, as ipfs-repo and
// ipfs-repo-migrations write them. They look like kubo's: a version file, a
// config, flatfs blocks/ (next-to-last/2, .data files) and a leveldb
// datastore/. What differs:
//
//   - blocks are keyed by base32 CID up to repo 7 and by base32 multihash from
//     repo 8, like kubo's repos 7-11 and 12+;
//   - pins are kubo's pin DAG under /local/pins up to repo 8; from repo 9 they
//     are their own leveldb, pins/, one cbor record per pin keyed by
//     multihash: {depth: 0 or Infinity, version?, codec?, metadata?};
//   - the MFS root is the file local/filesroot up to repo 10, then the
//     datastore key /local/filesroot;
//   - js-ipfs's flatfs (datastore-fs) names every file with .data, so blocks/
//     has SHARDING.data where kubo's has SHARDING. Older js-ipfs repos may
//     have neither SHARDING.data nor a datastore_spec, which kubo writes from
//     repo 6 on.
const (
	jsMinVersion Version = 7
	jsMaxVersion Version = 12
)

type jsRepo struct {
	*fsRepo
	jsv  Version
	pins kv // repo 9+
}

// isJSIPFS says whether the fs-repo at path, of version v, was written by
// js-ipfs: version 7 or later, with blocks/SHARDING.data or no
// datastore_spec.
func isJSIPFS(path string, v Version) bool {
	if v < jsMinVersion {
		return false
	}
	if _, err := os.Stat(filepath.Join(path, "blocks", "SHARDING.data")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(path, "datastore_spec")); err == nil {
		return false
	}
	_, err := os.Stat(filepath.Join(path, "blocks"))
	return err == nil
}

func openJSIPFS(path string, v Version) (Repo, error) {
	if v > jsMaxVersion {
		return nil, fmt.Errorf("%s: js-ipfs repo %d is newer than %d: %w", path, v, jsMaxVersion, ErrUnsupported)
	}
	// Block keys as kubo's repo with the same layout has them.
	kv := Version(7)
	if v >= 8 {
		kv = 12
	}
	blocks, err := openJSFlatfs(filepath.Join(path, "blocks"))
	if err != nil {
		return nil, err
	}
	ds, err := openLevelDB(filepath.Join(path, "datastore"))
	if err != nil {
		return nil, err
	}
	r := &jsRepo{fsRepo: &fsRepo{path: path, v: kv, blockPrefix: "/blocks/",
		ds: newMount([]mountPoint{{"/blocks", blocks}, {"/", ds}})}, jsv: v}
	if v >= 9 {
		if r.pins, err = openLevelDB(filepath.Join(path, "pins")); err != nil {
			// A repo that never pinned anything may have no pins/ yet.
			if _, serr := os.Stat(filepath.Join(path, "pins")); !errors.Is(serr, os.ErrNotExist) {
				r.ds.Close()
				return nil, err
			}
		}
	}
	return r, nil
}

// openJSFlatfs reads js-ipfs's blocks/: datastore-fs writes the shard id to
// SHARDING.data (older versions: SHARDING), and next-to-last/2 is the default.
func openJSFlatfs(dir string) (*flatfs, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("blocks directory %s: %w", dir, err)
	}
	f := &flatfs{dir: dir, shard: shardNextToLast(2)}
	for _, name := range []string{"SHARDING.data", "SHARDING"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if f.shard, err = parseShardFunc(strings.TrimSpace(string(b))); err != nil {
			return nil, fmt.Errorf("flatfs at %s: %w", dir, err)
		}
		break
	}
	return f, nil
}

func (r *jsRepo) Kind() string     { return KindJSIPFS }
func (r *jsRepo) Version() Version { return r.jsv }

func (r *jsRepo) Close() error {
	err := r.fsRepo.Close()
	if r.pins != nil {
		err = errors.Join(err, r.pins.Close())
	}
	return err
}

func (r *jsRepo) MFSRoot(ctx context.Context) (cid.Cid, bool, error) {
	if r.jsv >= 11 {
		return r.fsRepo.MFSRoot(ctx)
	}
	b, err := os.ReadFile(filepath.Join(r.path, "local", "filesroot"))
	if errors.Is(err, os.ErrNotExist) {
		return cid.Undef, false, nil
	}
	if err != nil {
		return cid.Undef, false, err
	}
	c, err := cid.Cast(b)
	if err != nil {
		return cid.Undef, false, fmt.Errorf("MFS root local/filesroot is not a CID: %w", err)
	}
	return c, true, nil
}

func (r *jsRepo) Pins(ctx context.Context) ([]Pin, error) {
	if r.jsv <= 8 {
		ps, err := r.dagPins(ctx) // the pin DAG, as kubo's repos 3-10 have it
		if err != nil {
			return nil, fmt.Errorf("reading pins: %w", err)
		}
		sortPins(ps)
		return ps, nil
	}
	var ps []Pin
	if r.pins == nil {
		return ps, nil
	}
	var keys []string
	if err := r.pins.Query("/", func(k string, _ int) error {
		keys = append(keys, k)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("reading pins: %w", err)
	}
	for _, k := range keys {
		raw, err := b32.DecodeString(strings.TrimPrefix(k, "/"))
		if err != nil {
			return nil, fmt.Errorf("pin key %q: %w", k, err)
		}
		mh, err := multihash.Cast(raw)
		if err != nil {
			return nil, fmt.Errorf("pin key %q: %w", k, err)
		}
		b, err := r.pins.Get(k)
		if err != nil {
			return nil, err
		}
		var rec struct {
			Depth    float64        `cbor:"depth"`
			Version  *uint64        `cbor:"version"`
			Codec    *uint64        `cbor:"codec"`
			Metadata map[string]any `cbor:"metadata"`
		}
		if err := cbor.Unmarshal(b, &rec); err != nil {
			return nil, fmt.Errorf("pin %s: %w", k, err)
		}
		codec := uint64(cid.DagProtobuf)
		if rec.Codec != nil {
			codec = *rec.Codec
		}
		c := cid.NewCidV1(codec, mh)
		if (rec.Version == nil || *rec.Version == 0) && codec == cid.DagProtobuf {
			c = cid.NewCidV0(mh)
		}
		p := Pin{Root: c, Mode: Direct}
		if math.IsInf(rec.Depth, 1) {
			p.Mode = Recursive
		} else if rec.Depth != 0 {
			return nil, fmt.Errorf("pin %s has depth %v; only 0 and Infinity are used", c, rec.Depth)
		}
		// js-ipfs pins have no names; metadata may carry one.
		if n, ok := rec.Metadata["name"].(string); ok {
			p.Name = n
		}
		ps = append(ps, p)
	}
	sortPins(ps)
	return ps, nil
}
