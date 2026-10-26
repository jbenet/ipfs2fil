// Package collection keeps a list of IPFS repos archived together. Every
// archive in a collection stores into the same data sets (one per copy), and
// packing a repo leaves out blocks that an earlier repo's pieces already hold,
// so blocks shared between repos are stored and paid for once.
//
// The list lives in repos.json in the collection's directory, by default
// ~/.ipfs2fil, with one archive directory per repo under archives/. It records
// repo paths, so it stays on this machine; nothing in it is uploaded.
package collection

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// File is the collection's list of repos, in its directory.
const File = "repos.json"

// EnvHome overrides the default collection directory.
const EnvHome = "IPFS2FIL_HOME"

// DefaultKey is the data set key of a new collection. Data sets are found by
// key among the wallet's own, so every machine using the same wallet and
// key shares the same data sets.
const DefaultKey = "repos:default"

type Collection struct {
	Dir    string `json:"-"`
	Format int    `json:"format"`
	// Key is the store key every archive in the collection uses
	// (manifest.Collection), so they share one set of data sets.
	Key   string `json:"key"`
	Repos []Repo `json:"repos"` // in the order added; earlier repos never depend on later ones
}

type Repo struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"` // the IPFS repo, absolute
	Out   string    `json:"out"`  // the archive directory, relative to the collection's
	Added time.Time `json:"added"`
}

// DefaultDir is $IPFS2FIL_HOME, else ~/.ipfs2fil.
func DefaultDir() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding your home directory: %w; set $%s", err, EnvHome)
	}
	return filepath.Join(home, ".ipfs2fil"), nil
}

// Open reads the collection in dir. A directory with no repos.json is an
// empty collection; it is written on the first Save.
func Open(dir string) (*Collection, error) {
	c := &Collection{Dir: dir, Format: 1, Key: DefaultKey}
	b, err := os.ReadFile(filepath.Join(dir, File))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, File), err)
	}
	if c.Key == "" {
		c.Key = DefaultKey
	}
	return c, nil
}

// Save writes repos.json atomically.
func (c *Collection) Save() error {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(c.Dir, File+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(c.Dir, File))
}

var nameOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Add appends a repo. path should be the repo's absolute path.
func (c *Collection) Add(name, path string, now time.Time) (*Repo, error) {
	if !nameOK.MatchString(name) {
		return nil, fmt.Errorf("%q is not a usable name; use letters, digits, '.', '_' and '-' (pass --name)", name)
	}
	for _, r := range c.Repos {
		if r.Name == name {
			return nil, fmt.Errorf("a repo named %q is already in the list (%s); pass --name to pick another", name, r.Path)
		}
		if r.Path == path {
			return nil, fmt.Errorf("%s is already in the list as %q", path, r.Name)
		}
	}
	c.Repos = append(c.Repos, Repo{Name: name, Path: path, Out: filepath.Join("archives", name), Added: now.UTC().Truncate(time.Second)})
	return &c.Repos[len(c.Repos)-1], nil
}

// Find returns the index of the named repo, or -1.
func (c *Collection) Find(name string) int {
	for i, r := range c.Repos {
		if r.Name == name {
			return i
		}
	}
	return -1
}

// OutDir is the repo's archive directory.
func (c *Collection) OutDir(r Repo) string {
	if filepath.IsAbs(r.Out) {
		return r.Out
	}
	return filepath.Join(c.Dir, r.Out)
}

// Manifest reads the repo's archive manifest, nil if it was never packed.
func (c *Collection) Manifest(r Repo) (*manifest.Manifest, error) {
	m, err := manifest.Read(filepath.Join(c.OutDir(r), archive.ManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return m, err
}

// Dependents lists the repos whose archives leave blocks to the named repo's
// pieces.
func (c *Collection) Dependents(name string) ([]string, error) {
	i := c.Find(name)
	if i < 0 {
		return nil, fmt.Errorf("no repo named %q; 'ipfs2fil repos ls' lists them", name)
	}
	m, err := c.Manifest(c.Repos[i])
	if err != nil || m == nil || len(m.Snapshots) == 0 {
		return nil, err
	}
	var out []string
	for _, r := range c.Repos {
		o, err := c.Manifest(r)
		if err != nil {
			return nil, err
		}
		if o == nil || o.Shared == nil {
			continue
		}
		for _, p := range o.Shared.Pieces {
			if p.Archive == m.Snapshots[0].Root {
				out = append(out, r.Name)
				break
			}
		}
	}
	return out, nil
}

// Shared is the archive.Options.Shared lookup for packing repo i: the data
// pieces of every earlier repo packed into this collection's data sets.
func (c *Collection) Shared(i int) (func(multihash.Multihash) (archive.SharedBlock, bool), error) {
	held := map[string]archive.SharedBlock{}
	for _, r := range c.Repos[:i] {
		m, err := c.Manifest(r)
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", r.Name, err)
		}
		if m == nil || m.Collection != c.Key || len(m.Snapshots) == 0 {
			continue // not packed yet, or stored in data sets of its own
		}
		for _, p := range m.Pieces {
			if p.BlockList == "" {
				continue
			}
			sb := archive.SharedBlock{Archive: m.Snapshots[0].Root, PieceCID: p.PieceCID}
			err := archive.ReadBlockList(filepath.Join(c.OutDir(r), "index", p.BlockList), func(h multihash.Multihash) error {
				if _, ok := held[string(h)]; !ok {
					held[string(h)] = sb
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("repo %s: reading its block list: %w (run 'ipfs2fil pack --all' to rewrite it)", r.Name, err)
			}
		}
	}
	return func(h multihash.Multihash) (archive.SharedBlock, bool) {
		sb, ok := held[string(h)]
		return sb, ok
	}, nil
}
