package collection

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Entry is one repo in the collection, on this machine or (from the
// catalog) on another.
type Entry struct {
	Repo
	Host  string // "" on this machine when there is no catalog
	Local bool   // on this machine: its archive, pieces and repo are here
	// Dir holds the archive's manifest.json and index/: the archive itself
	// for a local repo, its catalog copy for another machine's.
	Dir string
}

// ID names the entry across machines: host/name, or name alone locally
// without a catalog.
func (e Entry) ID() string {
	if e.Host == "" {
		return e.Name
	}
	return e.Host + "/" + e.Name
}

// Manifest reads the entry's manifest, nil if it was never packed.
func (e Entry) Manifest() (*manifest.Manifest, error) {
	m, err := manifest.Read(filepath.Join(e.Dir, archive.ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return m, err
}

// Entries lists every repo: this machine's, then with a catalog every other
// machine's, all ordered by when they were added (ties: list order, then host).
// That order decides which repos may rely on which: only on earlier ones.
func (c *Collection) Entries() ([]Entry, error) {
	var out []Entry
	for _, r := range c.Repos {
		out = append(out, Entry{Repo: r, Host: c.Host, Local: true, Dir: c.OutDir(r)})
	}
	if c.Catalog == "" {
		for i := range out {
			out[i].Host = ""
		}
		return out, nil
	}
	hosts, err := os.ReadDir(c.Catalog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, h := range hosts {
		if !h.IsDir() || h.Name() == c.Host || !hostOK.MatchString(h.Name()) {
			continue
		}
		o, err := Open(filepath.Join(c.Catalog, h.Name()))
		if err != nil {
			return nil, fmt.Errorf("the catalog's list for %s: %w", h.Name(), err)
		}
		for _, r := range o.Repos {
			out = append(out, Entry{Repo: r, Host: h.Name(), Dir: filepath.Join(c.Catalog, h.Name(), r.Name)})
		}
	}
	slices.SortStableFunc(out, func(a, b Entry) int {
		if n := a.Added.Compare(b.Added); n != 0 || a.Host == b.Host {
			return n
		}
		return strings.Compare(a.Host, b.Host)
	})
	return out, nil
}

// Publish copies a local repo's manifest and block lists into the catalog,
// so other machines see it. Without a catalog it does nothing.
func (c *Collection) Publish(r Repo) error {
	if c.Catalog == "" {
		return nil
	}
	src, dst := c.OutDir(r), filepath.Join(c.Catalog, c.Host, r.Name)
	m, err := manifest.Read(filepath.Join(src, archive.ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range m.Pieces {
		if p.BlockList == "" {
			continue
		}
		keep[filepath.Base(p.BlockList)] = true
		b, err := os.ReadFile(filepath.Join(src, "index", p.BlockList))
		if err != nil {
			return err
		}
		to := filepath.Join(dst, "index", p.BlockList)
		if old, err := os.ReadFile(to); err == nil && string(old) == string(b) {
			continue // unchanged: leave it for the sync tool
		}
		if err := writeFile(to, b); err != nil {
			return err
		}
	}
	old, _ := os.ReadDir(filepath.Join(dst, "index", "pieces"))
	for _, f := range old {
		if !keep[f.Name()] {
			os.Remove(filepath.Join(dst, "index", "pieces", f.Name()))
		}
	}
	b, err := os.ReadFile(filepath.Join(src, archive.ManifestFile))
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dst, archive.ManifestFile), b)
}

// Unpublish removes a repo's copy from the catalog.
func (c *Collection) Unpublish(name string) error {
	if c.Catalog == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(c.Catalog, c.Host, name))
}

// Dependents lists the repos, on any machine, whose archives leave blocks to
// the named local repo's pieces.
func (c *Collection) Dependents(name string) ([]string, error) {
	i := c.Find(name)
	if i < 0 {
		return nil, fmt.Errorf("no repo named %q; 'ipfs2fil repos ls' lists them", name)
	}
	m, err := c.Manifest(c.Repos[i])
	if err != nil || m == nil || len(m.Snapshots) == 0 {
		return nil, err
	}
	es, err := c.Entries()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		o, err := e.Manifest()
		if err != nil {
			return nil, err
		}
		if o == nil || o.Shared == nil {
			continue
		}
		for _, p := range o.Shared.Pieces {
			if p.Archive == m.Snapshots[0].Root {
				out = append(out, e.ID())
				break
			}
		}
	}
	return out, nil
}

// Shared is the archive.Options.Shared lookup for packing the named local
// repo: the data pieces of every repo added before it that stores into this
// collection's data sets. A repo on this machine counts once packed, since
// 'push --all' pushes in order; another machine's counts only once its
// manifest says every copy holds the piece, since this machine cannot push
// it.
func (c *Collection) Shared(name string) (func(multihash.Multihash) (archive.SharedBlock, bool), error) {
	es, err := c.Entries()
	if err != nil {
		return nil, err
	}
	held := map[string]archive.SharedBlock{}
	for _, e := range es {
		if e.Local && e.Name == name {
			break
		}
		m, err := e.Manifest()
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", e.ID(), err)
		}
		if m == nil || m.Collection != c.Key || len(m.Snapshots) == 0 {
			continue // not packed yet, or stored in data sets of its own
		}
		for _, p := range m.Pieces {
			if p.BlockList == "" || !e.Local && !stored(m, p.Index) {
				continue
			}
			sb := archive.SharedBlock{Archive: m.Snapshots[0].Root, PieceCID: p.PieceCID}
			err := archive.ReadBlockList(filepath.Join(e.Dir, "index", p.BlockList), func(h multihash.Multihash) error {
				if _, ok := held[string(h)]; !ok {
					held[string(h)] = sb
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("repo %s: reading its block list: %w (pack it again to rewrite it)", e.ID(), err)
			}
		}
	}
	return func(h multihash.Multihash) (archive.SharedBlock, bool) {
		sb, ok := held[string(h)]
		return sb, ok
	}, nil
}

// stored reports whether every copy m records holds piece i.
func stored(m *manifest.Manifest, i int) bool {
	if len(m.Copies) == 0 {
		return false
	}
	for _, c := range m.Copies {
		if c.PieceIDs[i] == "" {
			return false
		}
	}
	return true
}

// HostName is a usable catalog name for this machine: its host name up to
// the first dot, with anything else turned into '-'.
func HostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this-machine"
	}
	h, _, _ = strings.Cut(h, ".")
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if n := strings.TrimLeft(b.String(), "-_"); n != "" {
		return n
	}
	return "this-machine"
}
