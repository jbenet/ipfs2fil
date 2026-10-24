// Package repo reads kubo and go-ipfs fs-repos of every version, 0 through 18,
// offline and strictly read-only. It never runs migrations and never takes the
// repo lock for writing, so a live daemon's repo can be read safely.
//
// Every version is reduced to the same view: blocks keyed by multihash, the pin
// set, and the MFS root. Version differences live behind Open.
package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// Version is the fs-repo version, read from the repo's "version" file.
// Repos made by go-ipfs 0.2 have no version file and are version 0.
type Version int

// Latest is the newest fs-repo version this package knows (kubo 0.38 and later).
const Latest Version = 18

// PinMode says how a root is pinned. Indirect pins are not listed; they are
// reachable from recursive pins.
type PinMode int

const (
	Recursive PinMode = iota
	Direct
)

func (m PinMode) String() string {
	if m == Direct {
		return "direct"
	}
	return "recursive"
}

// Pin is one pinned root. Name is the pin name where the repo stores one
// (dspinner, repo 11 and later), otherwise empty.
type Pin struct {
	Root cid.Cid
	Mode PinMode
	Name string
}

// Repo is a read-only view of one fs-repo.
type Repo interface {
	Path() string
	Version() Version

	// ForEachBlock calls fn once for every block in the blockstore, in no
	// particular order. It stops at the first error fn returns.
	ForEachBlock(ctx context.Context, fn func(mh multihash.Multihash, size int) error) error
	// Get returns a block's bytes by multihash. It returns ErrNotFound for a
	// block the repo does not hold. Callers verify the hash; Get does not.
	Get(ctx context.Context, mh multihash.Multihash) ([]byte, error)

	Pins(ctx context.Context) ([]Pin, error)
	// MFSRoot returns the root of the files API tree, if the repo has one
	// (repo 3 and later).
	MFSRoot(ctx context.Context) (cid.Cid, bool, error)

	Close() error
}

var (
	ErrNotFound    = errors.New("block not found")
	ErrNotRepo     = errors.New("not an IPFS repo")
	ErrUnsupported = errors.New("fs-repo version not supported yet")
)

// Open detects the repo's version and returns a reader for it.
func Open(path string) (Repo, error) {
	v, err := DetectVersion(path)
	if err != nil {
		return nil, err
	}
	return openVersion(path, v)
}

// openVersion picks the block and pin layout for v. The readers land per
// layout family: leveldb blocks (0-1), old flatfs keys (2-3), base32 flatfs (4),
// sharded flatfs with CID keys (5-11), sharded flatfs with multihash keys (12+);
// pins as datastore key lists (0-2), the pin DAG (3-10), dspinner (11+).
var openVersion = func(path string, v Version) (Repo, error) {
	return nil, ErrUnsupported
}

// Discover finds the repo to use: the explicit path if given, then $IPFS_PATH,
// then ~/.ipfs.
func Discover(explicit string) (string, error) {
	cands := []string{explicit, os.Getenv("IPFS_PATH")}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".ipfs"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if _, err := DetectVersion(c); err == nil {
			return c, nil
		} else if c == explicit {
			return "", err
		}
	}
	return "", ErrNotRepo
}
