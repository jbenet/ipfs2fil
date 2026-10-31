package atproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
)

// Files in an account's cache directory, as Fetch leaves them.
const (
	RepoFile     = "repo.car"
	BlobDir      = "blobs"
	DIDDocFile   = "did.json"
	AuditLogFile = "plc-audit-log.json"
	IdentityFile = "identity.json"
)

// Fetched says what Fetch got.
type Fetched struct {
	RepoBytes int64
	Blobs     int // blobs the PDS lists, all now in the cache but Missing
	New       int // blobs downloaded this time
	Missing   []string
}

// blobWorkers is how many blobs download at once.
const blobWorkers = 4

// Fetch downloads the account's repo, its blobs and its identity documents
// into dir. Blobs already in dir are kept, so later runs fetch only new
// ones. No login is needed: all of it is public.
func Fetch(ctx context.Context, c *http.Client, id *Identity, dir string, progress func(done, total int)) (*Fetched, error) {
	if err := os.MkdirAll(filepath.Join(dir, BlobDir), 0o755); err != nil {
		return nil, err
	}
	meta, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	writes := map[string][]byte{IdentityFile: meta, DIDDocFile: id.DIDDoc}
	if id.AuditLog != nil {
		writes[AuditLogFile] = id.AuditLog
	}
	for name, b := range writes {
		if err := writeFile(filepath.Join(dir, name), bytes.NewReader(b)); err != nil {
			return nil, err
		}
	}

	f := &Fetched{}
	q := url.Values{"did": {id.DID}}
	res, err := get(ctx, c, id.PDS+"/xrpc/com.atproto.sync.getRepo?"+q.Encode(), time.Hour)
	if err != nil {
		return nil, fmt.Errorf("fetching the repo of %s from %s: %w", id.Name(), id.PDS, err)
	}
	cr := &countReader{r: res.Body}
	err = writeFile(filepath.Join(dir, RepoFile), cr)
	res.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("fetching the repo of %s: %w", id.Name(), err)
	}
	f.RepoBytes = cr.n

	var all []string
	for cursor := ""; ; {
		q := url.Values{"did": {id.DID}, "limit": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Cursor string   `json:"cursor"`
			CIDs   []string `json:"cids"`
		}
		if err := getJSON(ctx, c, id.PDS+"/xrpc/com.atproto.sync.listBlobs?"+q.Encode(), &page); err != nil {
			return nil, fmt.Errorf("listing the blobs of %s: %w", id.Name(), err)
		}
		all = append(all, page.CIDs...)
		if page.Cursor == "" || len(page.CIDs) == 0 {
			break
		}
		cursor = page.Cursor
	}
	f.Blobs = len(all)

	var todo []cid.Cid
	for _, s := range all {
		bc, err := cid.Decode(s)
		if err != nil {
			return nil, fmt.Errorf("the PDS listed a bad blob CID %q", s)
		}
		if _, err := os.Stat(filepath.Join(dir, BlobDir, bc.String())); err != nil {
			todo = append(todo, bc)
		}
	}
	var (
		mu   sync.Mutex
		done int
		errs []error
		next = make(chan cid.Cid)
		wg   sync.WaitGroup
	)
	if progress != nil {
		progress(0, len(todo))
	}
	for range blobWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for bc := range next {
				err := fetchBlob(ctx, c, id, dir, bc)
				mu.Lock()
				switch {
				case errors.Is(err, ErrNotFound):
					f.Missing = append(f.Missing, bc.String())
				case err != nil:
					errs = append(errs, fmt.Errorf("blob %s: %w", bc, err))
				default:
					f.New++
				}
				done++
				if progress != nil {
					progress(done, len(todo))
				}
				mu.Unlock()
			}
		}()
	}
	for _, bc := range todo {
		if ctx.Err() != nil {
			break
		}
		next <- bc
	}
	close(next)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("fetching the blobs of %s: %w", id.Name(), errors.Join(errs...))
	}
	return f, nil
}

// fetchBlob downloads one blob and checks it against its CID.
func fetchBlob(ctx context.Context, c *http.Client, id *Identity, dir string, bc cid.Cid) error {
	q := url.Values{"did": {id.DID}, "cid": {bc.String()}}
	res, err := get(ctx, c, id.PDS+"/xrpc/com.atproto.sync.getBlob?"+q.Encode(), 30*time.Minute)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	got, err := bc.Prefix().Sum(data)
	if err != nil {
		return err
	}
	if !got.Equals(bc) {
		return fmt.Errorf("the PDS sent bytes that hash to %s", got)
	}
	return writeFile(filepath.Join(dir, BlobDir, bc.String()), bytes.NewReader(data))
}

// writeFile writes r to path through a temporary file, so a cut-off
// download never leaves a partial file behind.
func writeFile(path string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}
