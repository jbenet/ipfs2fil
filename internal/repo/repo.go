// Package repo reads the places IPFS blocks live: kubo and go-ipfs fs-repos of
// every version, 0 through 18, js-ipfs repos 7 through 12, CAR files, running kubo nodes, and Bluesky
// (atproto) accounts, whose repos are IPLD too. It reads strictly read-only:
// it never runs migrations and never takes the repo lock for writing, so a
// live daemon's repo can be read safely.
//
// Every source is reduced to the same view: blocks keyed by multihash, named
// roots (pins), and the MFS root. Format differences live behind Open.
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
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

// Kinds of repo Open reads.
const (
	KindFSRepo  = "fs-repo"
	KindCAR     = "car"
	KindRPC     = "kubo-rpc"
	KindATProto = "atproto"
	KindJSIPFS  = "js-ipfs"
)

// Repo is a read-only view of one repo.
type Repo interface {
	Path() string
	// Kind says what sort of repo this is, one of the Kind constants.
	Kind() string
	// Version is the repo format's version within its kind: the fs-repo
	// version for fs-repos, 0 where a kind has none.
	Version() Version

	// ForEachBlock calls fn once for every block in the blockstore, in no
	// particular order. It stops at the first error fn returns. Repos 5..11
	// key blocks by CID, so a block stored under two CIDs comes twice.
	ForEachBlock(ctx context.Context, fn func(mh multihash.Multihash, size int) error) error
	// Get returns a block's bytes by multihash. It returns ErrNotFound for a
	// block the repo does not hold. Callers verify the hash; Get does not.
	Get(ctx context.Context, mh multihash.Multihash) ([]byte, error)

	// Pins are the repo's named roots: kubo's pin set, a CAR file's roots.
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

// Open detects what path holds and returns a reader for it: an fs-repo
// directory, a CAR file, a directory of CAR files, or the http:// address of
// a kubo node's RPC API, or a Bluesky account (at://handle or a DID).
func Open(path string) (Repo, error) {
	if atproto.IsAccount(path) {
		return openATProto(path)
	}
	if isRPC(path) {
		return openRPC(path)
	}
	if isCAR(path) {
		return openCAR(path)
	}
	v, err := DetectVersion(path)
	if err != nil {
		return nil, err
	}
	if isJSIPFS(path, v) {
		return openJSIPFS(path, v)
	}
	return openVersion(path, v)
}

// Describe names a repo's kind and version for people: "fs-repo 12", "CAR".
func Describe(r Repo) string {
	if d, ok := r.(interface{ Describe() string }); ok {
		return d.Describe()
	}
	switch r.Kind() {
	case KindFSRepo:
		return fmt.Sprintf("fs-repo %d", r.Version())
	case KindCAR:
		return "CAR"
	case KindJSIPFS:
		return fmt.Sprintf("js-ipfs repo %d", r.Version())
	}
	return r.Kind()
}

// openVersion picks the block and pin layout for v. Layout families: leveldb
// blocks (0-1), old flatfs keys (2-3), base32 flatfs (4), sharded flatfs with
// CID keys (5-11), sharded flatfs with multihash keys (12+); pins as datastore
// key lists (0-2), the pin DAG (3-10), dspinner (11+). Repos 6 and later can
// put any of this on other datastores through config's Datastore.Spec.
var openVersion = openFS

// Discover finds the repo to use: the explicit path if given, then $IPFS_PATH,
// then ~/.ipfs, then ~/.go-ipfs (repos 0 and 1), then js-ipfs's ~/.jsipfs.
func Discover(explicit string) (string, error) {
	cands := []string{explicit, os.Getenv("IPFS_PATH")}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".ipfs"), filepath.Join(home, ".go-ipfs"), filepath.Join(home, ".jsipfs"))
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if c == explicit && (atproto.IsAccount(c) || isRPC(c) || isCAR(c)) {
			return c, nil
		}
		if _, err := DetectVersion(c); err == nil {
			return c, nil
		} else if c == explicit {
			return "", err
		}
	}
	return "", ErrNotRepo
}
