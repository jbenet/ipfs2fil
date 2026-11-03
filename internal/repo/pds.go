package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
)

// PDSFile names the direct pin that lists a PDS archive's accounts.
const PDSFile = "pds.json"

// PDSBlobsFile ends the name of the direct pin, one per account, that
// lists every blob the account's PDS served: blobs no record links any more
// are archived as orphans, and this says whose they are.
const PDSBlobsFile = "blobs.json"

// pdsRepo is every active account on one PDS. Each account is read as
// at:// would read it on its own (repo, blobs, DID document, PLC log), so
// its pins are named after its handle; a pds.json document, pinned
// directly, says which server they came from and which accounts it listed.
type pdsRepo struct {
	base string // https://host

	once  sync.Once
	err   error
	srv   *atproto.Server
	accts []*atprotoRepo
	doc   []byte // pds.json
	docMH multihash.Multihash
	extra map[string][]byte // blobs.json documents, by multihash
	pins  []Pin
	where map[string]*atprotoRepo // multihash -> the first account holding it
}

// PDSDoc is pds.json: the server and what happened to each account it
// listed. It holds no times, so an unchanged server archives the same.
type PDSDoc struct {
	Type     string       `json:"$type"` // "ipfs2fil.pds"
	Server   string       `json:"server"`
	DID      string       `json:"did"`
	Accounts []PDSAccount `json:"accounts"`
}

// PDSAccount is one account a PDS listed.
type PDSAccount struct {
	DID    string `json:"did"`
	Handle string `json:"handle,omitempty"`
	Rev    string `json:"rev,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Skipped says why the account is not in the archive: the server said
	// it is not active, it lives on another PDS now, or it failed to fetch.
	Skipped string `json:"skipped,omitempty"`
}

// pdsProbe is how long IsPDS waits for a server to describe itself.
var pdsProbe = 10 * time.Second

var pdsProbed sync.Map // base URL -> bool

// IsPDS says whether path names an atproto PDS: pds:https://host or
// pds://host, or an https URL with no path whose server answers
// com.atproto.server.describeServer.
func IsPDS(path string) bool {
	if strings.HasPrefix(path, "pds:") {
		return pdsBase(path) != ""
	}
	if !strings.HasPrefix(path, "https://") && !strings.HasPrefix(path, "http://") {
		return false
	}
	base := pdsBase(path)
	if base == "" {
		return false
	}
	if v, ok := pdsProbed.Load(base); ok {
		return v.(bool)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdsProbe)
	defer cancel()
	_, err := atproto.DescribeServer(ctx, HTTPClient, base)
	pdsProbed.Store(base, err == nil)
	return err == nil
}

// pdsBase turns the forms IsPDS takes into https://host, or "" if path
// is not one of them. A URL with a path, query or user is not a PDS.
func pdsBase(path string) string {
	s := strings.TrimPrefix(path, "pds:")
	if strings.HasPrefix(s, "//") {
		s = "https:" + s
	} else if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || strings.Trim(u.Path, "/") != "" {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

func openPDS(path string) (Repo, error) {
	base := pdsBase(path)
	if base == "" {
		return nil, ErrNotRepo
	}
	return &pdsRepo{base: base}, nil
}

func (r *pdsRepo) Path() string     { return "pds:" + r.base }
func (r *pdsRepo) Kind() string     { return KindPDS }
func (r *pdsRepo) Version() Version { return 0 }
func (r *pdsRepo) Close() error     { return nil }

func (r *pdsRepo) Describe() string {
	host := strings.TrimPrefix(strings.TrimPrefix(r.base, "https://"), "http://")
	if err := r.load(); err != nil {
		return "Bluesky PDS " + host
	}
	return fmt.Sprintf("Bluesky PDS %s, %d accounts", host, len(r.accts))
}

func (r *pdsRepo) load() error {
	r.once.Do(func() { r.err = r.fetch() })
	return r.err
}

// pdsWorkers is how many accounts of a PDS fetch at once.
const pdsWorkers = 4

func (r *pdsRepo) fetch() error {
	ctx := context.Background()
	srv, err := atproto.DescribeServer(ctx, HTTPClient, r.base)
	if err != nil {
		return fmt.Errorf("%s is not a PDS, or did not answer: %w", r.base, err)
	}
	r.srv = srv
	listed, err := atproto.ListRepos(ctx, HTTPClient, r.base)
	if err != nil {
		return err
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].DID < listed[j].DID })
	logf("%s hosts %d accounts\n", r.base, len(listed))

	doc := PDSDoc{Type: "ipfs2fil.pds", Server: r.base, DID: srv.DID, Accounts: make([]PDSAccount, len(listed))}
	accts := make([]*atprotoRepo, len(listed))
	var wg sync.WaitGroup
	sem := make(chan struct{}, pdsWorkers)
	for i, h := range listed {
		doc.Accounts[i] = PDSAccount{DID: h.DID}
		if !h.IsActive() {
			st := h.Status
			if st == "" {
				st = "inactive"
			}
			doc.Accounts[i].Skipped = "the server says it is " + st
			continue
		}
		wg.Add(1)
		go func(i int, did string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a, _ := openATProto(did)
			ar := a.(*atprotoRepo)
			if err := ar.load(); err != nil {
				doc.Accounts[i].Skipped = "fetch failed: " + err.Error()
				return
			}
			if !sameHost(ar.id.PDS, r.base) {
				doc.Accounts[i].Skipped = "it lives on " + ar.id.PDS + " now"
				return
			}
			doc.Accounts[i].Handle, doc.Accounts[i].Rev, doc.Accounts[i].Commit = ar.id.Handle, ar.rev, ar.root.String()
			accts[i] = ar
		}(i, h.DID)
	}
	wg.Wait()

	failed := 0
	r.extra = map[string][]byte{}
	for i, a := range accts {
		if a != nil {
			r.accts = append(r.accts, a)
			pins, _ := a.Pins(ctx)
			r.pins = append(r.pins, pins...)
			b, err := blobList(a)
			if err != nil {
				return err
			}
			mh := sha256MH(b)
			r.extra[string(mh)] = b
			r.pins = append(r.pins, Pin{Root: cid.NewCidV1(cid.Raw, mh), Mode: Direct, Name: a.id.Name() + " " + PDSBlobsFile})
		} else if s := doc.Accounts[i].Skipped; strings.HasPrefix(s, "fetch failed") {
			failed++
			logf("  skipped %s: %s\n", doc.Accounts[i].DID, s)
		}
	}
	if failed > 0 {
		logf("  %d of %d accounts failed to fetch; pds.json in the archive names them\n", failed, len(listed))
	}
	if len(r.accts) == 0 && failed > 0 {
		return fmt.Errorf("%s: every account failed to fetch", r.base)
	}
	r.where = map[string]*atprotoRepo{}
	for _, a := range r.accts {
		err := a.ForEachBlock(ctx, func(mh multihash.Multihash, _ int) error {
			if _, ok := r.where[string(mh)]; !ok {
				r.where[string(mh)] = a
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if r.doc, err = json.MarshalIndent(doc, "", "  "); err != nil {
		return err
	}
	r.docMH = sha256MH(r.doc)
	r.pins = append(r.pins, Pin{Root: cid.NewCidV1(cid.Raw, r.docMH), Mode: Direct, Name: PDSFile})
	return nil
}

func sha256MH(b []byte) multihash.Multihash {
	sum := sha256.Sum256(b)
	mh, _ := multihash.Encode(sum[:], multihash.SHA2_256)
	return mh
}

// blobList is an account's blobs.json: the CIDs of every blob its PDS
// listed and served, sorted.
func blobList(a *atprotoRepo) ([]byte, error) {
	cids := []string{}
	for k := range a.blobs {
		cids = append(cids, cid.NewCidV1(cid.Raw, multihash.Multihash(k)).String())
	}
	sort.Strings(cids)
	return json.MarshalIndent(map[string]any{"$type": "ipfs2fil.blobs", "did": a.id.DID, "blobs": cids}, "", "  ")
}

// sameHost says whether a DID document's PDS endpoint is base.
func sameHost(endpoint, base string) bool {
	u, err := url.Parse(endpoint)
	b, err2 := url.Parse(base)
	return err == nil && err2 == nil && strings.EqualFold(u.Host, b.Host)
}

// ForEachBlock lists each account's blocks, every block once: accounts
// can share blobs.
func (r *pdsRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	if err := r.load(); err != nil {
		return err
	}
	for _, a := range r.accts {
		err := a.ForEachBlock(ctx, func(mh multihash.Multihash, size int) error {
			if r.where[string(mh)] != a {
				return nil // listed by an earlier account
			}
			return fn(mh, size)
		})
		if err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(r.extra)+1)
	for k := range r.extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	keys = append(keys, string(r.docMH))
	for _, k := range keys {
		if _, ok := r.where[k]; ok {
			continue
		}
		if err := fn(multihash.Multihash(k), len(r.getDoc(k))); err != nil {
			return err
		}
	}
	return nil
}

func (r *pdsRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	if b := r.getDoc(string(mh)); b != nil {
		return b, nil
	}
	if a, ok := r.where[string(mh)]; ok {
		return a.Get(ctx, mh)
	}
	return nil, ErrNotFound
}

func (r *pdsRepo) getDoc(k string) []byte {
	if k == string(r.docMH) {
		return r.doc
	}
	return r.extra[k]
}

// CARLayouts gives each account's repo CAR, named <DID>/repo.car as an
// export writes it.
func (r *pdsRepo) CARLayouts() ([]CARLayout, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	var out []CARLayout
	for _, a := range r.accts {
		ls, err := a.CARLayouts()
		if err != nil {
			return nil, err
		}
		for i := range ls {
			ls[i].Name = PDSAccountDir(a.id.DID) + "/" + ls[i].Name
		}
		out = append(out, ls...)
	}
	return out, nil
}

// PDSAccountDir names an account's folder in a PDS export: its DID, made
// safe for file names.
func PDSAccountDir(did string) string { return safeName(did) }

func (r *pdsRepo) Pins(context.Context) ([]Pin, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *pdsRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	return cid.Undef, false, nil
}
