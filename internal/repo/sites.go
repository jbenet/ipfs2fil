package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/codec/dagjson"
	"github.com/ipld/go-ipld-prime/node/basicnode"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

// SitesDir keeps what site lists last resolved to (last.json), so a site
// that cannot be reached keeps its last root. The CLI sets it under its
// home directory.
var SitesDir = filepath.Join(os.TempDir(), "ipfs2fil-sites")

// SitesFile names the direct pin that says where each site's root came
// from.
const SitesFile = "sites.json"

// SiteResolver finds sites' roots. Tests swap its parts.
var SiteResolver = &sitelist.Resolver{}

// IsSites says whether path names a site list: a file named like one
// (x.ipfs-sites.toml, ipfs-sites.toml, or the older x.sites), or
// sites:<path> for one named otherwise.
func IsSites(path string) bool {
	if !IsSitesName(path) {
		return false
	}
	fi, err := os.Stat(strings.TrimPrefix(path, "sites:"))
	return err == nil && fi.Mode().IsRegular()
}

// IsSitesName says whether path is named like a site list, without
// looking at the file.
func IsSitesName(path string) bool {
	return strings.HasPrefix(path, "sites:") || sitelist.IsListName(path)
}

// sitesRepo is a site archive: the websites a site list names, each
// resolved to the root it serves now and fetched whole from gateways, as a
// recursive pin under the site's name. Each run that finds a new root adds
// a snapshot, so a site's history builds up in the archive.
type sitesRepo struct {
	path string // the list file

	once  sync.Once
	err   error
	list  *sitelist.List
	car   *carRepo
	docs  map[string][]byte // sites.json and the list, by multihash
	order []string
	pins  []Pin
}

// SitesDoc is sites.json.
type SitesDoc struct {
	Type    string      `json:"$type"` // "ipfs2fil.sites"
	Title   string      `json:"title,omitempty"`
	List    string      `json:"list,omitempty"` // the list's file name
	Checked string      `json:"checked,omitempty"`
	Sites   []SiteEntry `json:"sites"`
}

// SiteEntry is one site of the list: the root it is archived at and how
// that root was found.
type SiteEntry = sitelist.Entry

// SitesDocBytes encodes sites.json as DAG-JSON: each site found has its
// root as a string (root, as before) and as an IPLD link (link), so the
// document is an IPLD node whose links are the sites. Pinning it
// recursively (ipfs pin add) pins every site's current root. Earlier roots
// stay strings: they may not be stored anywhere any more.
func SitesDocBytes(d SitesDoc) ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if ss, ok := m["sites"].([]any); ok {
		for _, x := range ss {
			if e, ok := x.(map[string]any); ok {
				if root, _ := e["root"].(string); root != "" {
					if _, err := cid.Decode(root); err == nil {
						e["link"] = map[string]any{"/": root}
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	// canonical DAG-JSON: sorted keys, no spaces, links as links
	nb := basicnode.Prototype.Any.NewBuilder()
	if err := dagjson.Decode(nb, &buf); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := dagjson.Encode(nb.Build(), &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func openSites(path string) (Repo, error) {
	p := strings.TrimPrefix(path, "sites:")
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	return &sitesRepo{path: abs}, nil
}

func (r *sitesRepo) Path() string     { return r.path }
func (r *sitesRepo) Kind() string     { return KindSites }
func (r *sitesRepo) Version() Version { return 0 }

func (r *sitesRepo) Close() error {
	if r.car != nil {
		return r.car.Close()
	}
	return nil
}

func (r *sitesRepo) Describe() string {
	if err := r.load(); err != nil {
		return "site list"
	}
	return fmt.Sprintf("site list, %d sites", len(r.list.Sites))
}

func (r *sitesRepo) load() error {
	r.once.Do(func() { r.err = r.fetch() })
	return r.err
}

// siteWorkers is how many sites resolve and fetch at once.
const siteWorkers = 4

func (r *sitesRepo) fetch() error {
	ctx := context.Background()
	var err error
	if r.list, err = sitelist.Load(r.path); err != nil {
		return err
	}
	// What each site served last: the lock file next to the list, else
	// (for a line-format list, or before its first run) what earlier runs
	// kept under SitesDir.
	lockPath := sitelist.LockPath(r.path)
	lock := &sitelist.Lock{}
	if lockPath != "" {
		if lock, err = sitelist.ReadLock(lockPath); err != nil {
			return err
		}
	}
	lastFile := filepath.Join(SitesDir, "last.json")
	last := map[string]string{} // source -> root
	if b, err := os.ReadFile(lastFile); err == nil {
		json.Unmarshal(b, &last)
	}
	if len(lock.Site) == 0 {
		for _, s := range r.list.Sites {
			if root, ok := last[s.Source]; ok {
				lock.Site = append(lock.Site, SiteEntry{Site: s, Root: root})
			}
		}
	}
	res := *SiteResolver
	if res.HTTP == nil {
		res.HTTP = HTTPClient
	}
	f := fetcher()
	if len(r.list.Gateways) > 0 {
		f.Gateways = r.list.Gateways
	}
	if len(res.Gateways) == 0 {
		res.Gateways = f.Gateways
	}
	lock = sitelist.Update(ctx, r.list, lock, sitelist.Options{
		Resolver: &res, Workers: siteWorkers, Log: Log,
		Fetch: f.Fetch, Have: f.Have,
	})
	for _, e := range lock.Site {
		if e.Root != "" {
			last[e.Source] = e.Root
		}
	}
	if b, err := json.MarshalIndent(last, "", "  "); err == nil {
		if os.MkdirAll(SitesDir, 0o755) == nil {
			os.WriteFile(lastFile, b, 0o644)
		}
	}
	if lockPath != "" {
		if err := lock.Write(lockPath); err != nil {
			logf("  could not write %s: %v\n", lockPath, err)
		}
	}
	entries := lock.Site

	r.car = &carRepo{path: r.path, at: map[string]carBlock{}}
	indexed := map[string]bool{}
	failed := 0
	for _, e := range entries {
		if e.Root == "" {
			failed++
			continue
		}
		c, _ := cid.Decode(e.Root)
		r.pins = append(r.pins, Pin{Root: c, Mode: Recursive, Name: e.Name})
		if f := f.File(c); !indexed[f] {
			indexed[f] = true
			if err := r.car.index(f, filepath.Dir(f)); err != nil {
				return err
			}
		}
	}
	r.car.pins = nil
	if failed == len(entries) {
		return errors.New("no site in the list could be resolved and fetched")
	}
	if failed > 0 {
		logf("  %d of %d sites are not in this snapshot; sites.json says why\n", failed, len(entries))
	}
	doc, err := SitesDocBytes(SitesDoc{Type: "ipfs2fil.sites", Title: r.list.Title, List: filepath.Base(r.path), Checked: lock.Checked.Format(time.RFC3339), Sites: entries})
	if err != nil {
		return err
	}
	r.docs = map[string][]byte{}
	for _, d := range []struct {
		name string
		b    []byte
	}{{SitesFile, doc}, {filepath.Base(r.path), r.list.Raw}} {
		sum := sha256.Sum256(d.b)
		mh, _ := multihash.Encode(sum[:], multihash.SHA2_256)
		if _, ok := r.docs[string(mh)]; !ok {
			r.docs[string(mh)] = d.b
			r.order = append(r.order, string(mh))
		}
		codec := uint64(cid.Raw)
		if d.name == SitesFile {
			codec = cid.DagJSON // its sites' roots are links
		}
		r.pins = append(r.pins, Pin{Root: cid.NewCidV1(codec, mh), Mode: Direct, Name: d.name})
	}
	return nil
}

func (r *sitesRepo) ForEachBlock(ctx context.Context, fn func(multihash.Multihash, int) error) error {
	if err := r.load(); err != nil {
		return err
	}
	if err := r.car.ForEachBlock(ctx, fn); err != nil {
		return err
	}
	for _, k := range r.order {
		if _, ok := r.car.at[k]; ok {
			continue
		}
		if err := fn(multihash.Multihash(k), len(r.docs[k])); err != nil {
			return err
		}
	}
	return nil
}

func (r *sitesRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	if b, ok := r.docs[string(mh)]; ok {
		return b, nil
	}
	return r.car.Get(ctx, mh)
}

func (r *sitesRepo) Pins(context.Context) ([]Pin, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *sitesRepo) MFSRoot(context.Context) (cid.Cid, bool, error) {
	return cid.Undef, false, nil
}
