package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
)

// ATProtoDir is where Bluesky (atproto) accounts are fetched to, one
// directory per DID. The CLI sets it under its home directory.
var ATProtoDir = filepath.Join(os.TempDir(), "ipfs2fil-atproto")

// Log, if set, hears what readers do that takes a while, such as fetching
// an account.
var Log io.Writer

// HTTPClient makes the readers' web requests.
var HTTPClient = http.DefaultClient

func logf(format string, args ...any) {
	if Log != nil {
		fmt.Fprintf(Log, format, args...)
	}
}

// atprotoRepo is a Bluesky (atproto) account: its repo, every blob it lists
// and its identity documents, fetched on first use into ATProtoDir. The
// commit is a recursive pin named after the account; records link their
// blobs, so walking the commit reaches them, and blobs no record links
// any more come along as orphans. The DID document and PLC log are direct
// pins, so the archive holds what it takes to check the commit's signature.
type atprotoRepo struct {
	acct string // handle or DID, as given

	once sync.Once
	err  error
	id   *atproto.Identity
	car  *carRepo
	root cid.Cid
	rev  string
	// blobs and docs are blocks beside the repo CAR: blob files by
	// multihash, identity documents in memory.
	blobs map[string]string
	docs  map[string][]byte
	order []string
	pins  []Pin
}

// accounts keeps what this process fetched, so the scan, pack and push of
// one run fetch an account once.
var accounts sync.Map // account -> *atprotoRepo

func openATProto(path string) (Repo, error) {
	acct := atproto.Account(path)
	if acct == "" {
		return nil, ErrNotRepo
	}
	if r, ok := accounts.Load(acct); ok {
		return r.(*atprotoRepo), nil
	}
	r := &atprotoRepo{acct: acct}
	accounts.Store(acct, r)
	return r, nil
}

func (r *atprotoRepo) Path() string     { return "at://" + r.acct }
func (r *atprotoRepo) Kind() string     { return KindATProto }
func (r *atprotoRepo) Version() Version { return 0 }

// Close keeps the repo open: the next Open of the account in this process
// reuses it.
func (r *atprotoRepo) Close() error { return nil }

func (r *atprotoRepo) Describe() string {
	if err := r.load(); err != nil || r.id == nil {
		return "Bluesky account"
	}
	return fmt.Sprintf("Bluesky account %s, rev %s", r.id.Name(), r.rev)
}

// load resolves the account and fetches it. With no network it falls back
// to what an earlier run fetched.
func (r *atprotoRepo) load() error {
	r.once.Do(func() { r.err = r.fetch() })
	return r.err
}

func (r *atprotoRepo) fetch() error {
	ctx := context.Background()
	id, err := atproto.Resolve(ctx, HTTPClient, r.acct)
	var dir string
	if err == nil {
		dir = filepath.Join(ATProtoDir, safeName(id.DID))
		logf("fetching %s from %s\n", id.Name(), id.PDS)
		var last int
		f, ferr := atproto.Fetch(ctx, HTTPClient, id, dir, func(done, total int) {
			if total > 0 && (done == total || done-last >= 100) {
				logf("  blobs %d/%d\n", done, total)
				last = done
			}
		})
		if ferr != nil {
			return ferr
		}
		if len(f.Missing) > 0 {
			logf("  %d blobs the server lists but no longer serves were skipped\n", len(f.Missing))
		}
	} else {
		// Offline: an earlier fetch, found by handle or DID.
		cached := r.cached()
		if cached == "" {
			return err
		}
		logf("could not reach %s (%v); using the copy fetched earlier\n", r.acct, err)
		dir = cached
		id = &atproto.Identity{}
		if err := readJSON(filepath.Join(dir, atproto.IdentityFile), id); err != nil {
			return err
		}
	}
	r.id = id
	return r.open(dir)
}

// cached finds an earlier fetch of the account in ATProtoDir.
func (r *atprotoRepo) cached() string {
	ds, _ := os.ReadDir(ATProtoDir)
	for _, d := range ds {
		dir := filepath.Join(ATProtoDir, d.Name())
		var id atproto.Identity
		if readJSON(filepath.Join(dir, atproto.IdentityFile), &id) == nil && (id.DID == r.acct || id.Handle == r.acct) {
			return dir
		}
	}
	return ""
}

// open reads a fetched account's directory.
func (r *atprotoRepo) open(dir string) error {
	c, err := openCAR(filepath.Join(dir, atproto.RepoFile))
	if err != nil {
		return fmt.Errorf("the repo fetched from %s: %w", r.id.PDS, err)
	}
	r.car = c.(*carRepo)
	if len(r.car.pins) != 1 {
		return fmt.Errorf("the repo of %s names %d roots, not 1", r.id.Name(), len(r.car.pins))
	}
	r.root = r.car.pins[0].Root
	data, err := r.car.Get(context.Background(), r.root.Hash())
	if err != nil {
		return err
	}
	cm, err := atproto.DecodeCommit(data)
	if err != nil {
		return fmt.Errorf("the repo of %s: %w", r.id.Name(), err)
	}
	if cm.DID != r.id.DID {
		return fmt.Errorf("the repo fetched for %s is signed for %s", r.id.DID, cm.DID)
	}
	r.rev = cm.Rev

	r.blobs, r.docs = map[string]string{}, map[string][]byte{}
	ents, err := os.ReadDir(filepath.Join(dir, atproto.BlobDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range ents {
		bc, err := cid.Decode(e.Name())
		if err != nil {
			continue // a temporary file
		}
		k := string(bc.Hash())
		if _, ok := r.car.at[k]; !ok {
			r.blobs[k] = filepath.Join(dir, atproto.BlobDir, e.Name())
			r.order = append(r.order, k)
		}
	}
	sort.Strings(r.order)

	r.pins = []Pin{{Root: r.root, Mode: Recursive, Name: r.id.Name()}}
	for _, name := range []string{atproto.DIDDocFile, atproto.AuditLogFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h, _ := multihash.Encode(sum[:], multihash.SHA2_256)
		k := string(h)
		if _, ok := r.docs[k]; !ok {
			r.docs[k] = b
			r.order = append(r.order, k)
		}
		r.pins = append(r.pins, Pin{Root: cid.NewCidV1(cid.Raw, h), Mode: Direct, Name: r.id.Name() + " " + name})
	}
	return nil
}

func (r *atprotoRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	if err := r.load(); err != nil {
		return err
	}
	if err := r.car.ForEachBlock(ctx, fn); err != nil {
		return err
	}
	for _, k := range r.order {
		size := len(r.docs[k])
		if p, ok := r.blobs[k]; ok {
			fi, err := os.Stat(p)
			if err != nil {
				return err
			}
			size = int(fi.Size())
		}
		if err := fn(multihash.Multihash(k), size); err != nil {
			return err
		}
	}
	return nil
}

func (r *atprotoRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	if p, ok := r.blobs[string(mh)]; ok {
		return os.ReadFile(p)
	}
	if b, ok := r.docs[string(mh)]; ok {
		return b, nil
	}
	return r.car.Get(ctx, mh)
}

func (r *atprotoRepo) Pins(context.Context) ([]Pin, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *atprotoRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	return cid.Undef, false, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// safeName makes a DID usable as a directory name on every system.
func safeName(did string) string {
	b := []byte(did)
	for i, c := range b {
		if c == ':' || c == '/' || c == '\\' || c == '%' {
			b[i] = '_'
		}
	}
	return string(b)
}
