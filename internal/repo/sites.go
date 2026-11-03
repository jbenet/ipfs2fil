package repo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ipfs/go-cid"
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

// SitesExt ends the name of a site list file.
const SitesExt = ".sites"

// SiteResolver finds sites' roots. Tests swap its parts.
var SiteResolver = &sitelist.Resolver{}

// IsSites says whether path names a site list: a file ending in .sites,
// or sites:<path> for one named otherwise.
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
	return strings.HasPrefix(path, "sites:") || strings.EqualFold(filepath.Ext(path), SitesExt)
}

// sitesRepo is a site archive: the websites a site list names, each
// resolved to the root it serves now and fetched whole from gateways, as a
// recursive pin under the site's name. Each run that finds a new root adds
// a snapshot, so a site's history builds up in the archive.
type sitesRepo struct {
	path string // the list file

	once  sync.Once
	err   error
	list  []byte
	sites []sitelist.Site
	car   *carRepo
	docs  map[string][]byte // sites.json and the list, by multihash
	order []string
	pins  []Pin
}

// SitesDoc is sites.json.
type SitesDoc struct {
	Type  string      `json:"$type"` // "ipfs2fil.sites"
	Sites []SiteEntry `json:"sites"`
}

// SiteEntry is one site of the list: the root it is archived at and how
// that root was found.
type SiteEntry struct {
	sitelist.Site
	Root string `json:"root,omitempty"`
	How  string `json:"how,omitempty"`
	// Stale says the site could not be resolved or fetched this time, so
	// the root is the one found last; Error says why.
	Stale bool   `json:"stale,omitempty"`
	Error string `json:"error,omitempty"`
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
	return fmt.Sprintf("site list, %d sites", len(r.sites))
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
	if r.list, err = os.ReadFile(r.path); err != nil {
		return err
	}
	if r.sites, err = sitelist.Parse(strings.NewReader(string(r.list))); err != nil {
		return fmt.Errorf("%s: %w", r.path, err)
	}
	last := map[string]string{} // source -> root
	lastFile := filepath.Join(SitesDir, "last.json")
	if b, err := os.ReadFile(lastFile); err == nil {
		json.Unmarshal(b, &last)
	}
	res := SiteResolver
	if res.HTTP == nil || len(res.Gateways) == 0 {
		cp := *res
		if cp.HTTP == nil {
			cp.HTTP = HTTPClient
		}
		if len(cp.Gateways) == 0 {
			cp.Gateways = Gateways
		}
		res = &cp
	}

	entries := make([]SiteEntry, len(r.sites))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, siteWorkers)
	for i, s := range r.sites {
		entries[i].Site = s
		wg.Add(1)
		go func(e *SiteEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			prev := last[e.Source]
			mu.Unlock()
			got, err := res.Resolve(ctx, e.Site)
			if err == nil {
				if err = fetchDAG(ctx, got.Root, dagFile(got.Root)); err == nil {
					e.Root, e.How = got.Root.String(), got.How
					mu.Lock()
					last[e.Source] = e.Root
					mu.Unlock()
					logf("  %s: %s (%s)\n", e.Name, e.Root, e.How)
					return
				}
			}
			e.Error = err.Error()
			if c, perr := cid.Decode(prev); perr == nil {
				if _, serr := os.Stat(dagFile(c)); serr == nil {
					e.Root, e.Stale = prev, true
					logf("  %s: %v; keeping the root found last, %s\n", e.Name, err, prev)
					return
				}
			}
			logf("  %s: %v\n", e.Name, err)
		}(&entries[i])
	}
	wg.Wait()
	if b, err := json.MarshalIndent(last, "", "  "); err == nil {
		if os.MkdirAll(SitesDir, 0o755) == nil {
			os.WriteFile(lastFile, b, 0o644)
		}
	}

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
		if f := dagFile(c); !indexed[f] {
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
	doc, err := json.MarshalIndent(SitesDoc{Type: "ipfs2fil.sites", Sites: entries}, "", "  ")
	if err != nil {
		return err
	}
	r.docs = map[string][]byte{}
	for _, d := range []struct {
		name string
		b    []byte
	}{{SitesFile, doc}, {filepath.Base(r.path), r.list}} {
		sum := sha256.Sum256(d.b)
		mh, _ := multihash.Encode(sum[:], multihash.SHA2_256)
		if _, ok := r.docs[string(mh)]; !ok {
			r.docs[string(mh)] = d.b
			r.order = append(r.order, string(mh))
		}
		r.pins = append(r.pins, Pin{Root: cid.NewCidV1(cid.Raw, mh), Mode: Direct, Name: d.name})
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
