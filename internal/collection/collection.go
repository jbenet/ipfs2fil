// Package collection keeps a list of IPFS repos archived together. Every
// archive in a collection stores into the same data sets (one per copy), and
// packing a repo leaves out blocks that an earlier repo's pieces already hold,
// so blocks shared between repos are stored and paid for once.
//
// The list lives in repos.json in the collection's directory, by default
// ~/.ipfs2fil, with one archive directory per repo under archives/. It records
// repo paths, so it stays on this machine; nothing in it is uploaded.
//
// Repos spread over several machines share a catalog: a directory every
// machine can see, such as a synced folder or a git checkout (catalog.go).
// Each machine keeps its list there under its own host name, with a copy of
// each archive's manifest and block lists, so any machine can show every
// repo and leave out blocks another machine's pieces already hold.
package collection

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// File is the collection's list of repos, in its directory.
const File = "repos.json"

// Environment variables for the collection directory, the catalog shared by
// every machine, and this machine's name in it.
const (
	EnvHome    = "IPFS2FIL_HOME"
	EnvCatalog = "IPFS2FIL_CATALOG"
	EnvHost    = "IPFS2FIL_HOST"
)

// DefaultKey is the data set key of a new collection. Data sets are found by
// key among the wallet's own, so every machine using the same wallet and
// key shares the same data sets.
const DefaultKey = "repos:default"

type Collection struct {
	Dir string `json:"-"` // the archives (and, with no catalog, the list) live here
	// Catalog, if set, is the directory shared by every machine; this
	// machine's list is Catalog/Host/repos.json.
	Catalog string      `json:"-"`
	Host    string      `json:"-"`
	git     *gitCatalog // set when the catalog is a git repository
	chain   bool        // the catalog is read from chain (chain.go); Catalog is its cache

	Format int `json:"format"`
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

// Open reads the collection in dir alone, with no catalog. A directory with
// no repos.json is an empty collection; it is written on the first Save.
func Open(dir string) (*Collection, error) {
	return open(&Collection{Dir: dir, Format: 1, Key: DefaultKey}, "")
}

// OpenShared reads this machine's list, host's, and sets up the catalog that
// shows other machines' repos. With catalog "" or Chain, the catalog is read
// from chain (SyncChain) and the list stays in dir/repos.json. Otherwise the
// catalog is a folder or a git repository (IsGitURL, cloned into dir/catalog
// or pulled) and the list is catalog/host/repos.json; a machine joining it
// brings its local list along.
func OpenShared(dir, catalog, host string) (*Collection, error) {
	if !hostOK.MatchString(host) {
		return nil, fmt.Errorf("%q is not a usable host name for the catalog; pass --host with letters, digits, '.', '_' and '-'", host)
	}
	c := &Collection{Dir: dir, Catalog: catalog, Host: host, Format: 1, Key: DefaultKey}
	if catalog == "" || catalog == Chain {
		c.Catalog, c.chain = filepath.Join(dir, ChainDir), true
		return open(c, "")
	}
	return open(c, catalog)
}

func open(c *Collection, catalog string) (*Collection, error) {
	dir := c.Dir
	if catalog != "" && IsGitURL(catalog) {
		c.git = &gitCatalog{url: catalog, dir: filepath.Join(dir, CloneDir), host: c.Host}
		if err := c.git.sync(); err != nil {
			return nil, err
		}
		c.Catalog = c.git.dir
	}
	path := c.listPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && catalog != "" && !c.chain {
		path = filepath.Join(dir, File)
		b, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if c.Key == "" {
		c.Key = DefaultKey
	}
	return c, nil
}

func (c *Collection) listPath() string {
	if c.Catalog != "" && !c.chain {
		return filepath.Join(c.Catalog, c.Host, File)
	}
	return filepath.Join(c.Dir, File)
}

// Commit shares this machine's changes to the catalog: for a git catalog it
// commits and pushes them; for a folder, the folder is already shared; on
// chain, push shares them.
func (c *Collection) Commit(msg string) error {
	if c.git == nil {
		return nil
	}
	return c.git.commit(msg)
}

// Stale says why the catalog may be out of date (a git pull that failed),
// or "" if it is not.
func (c *Collection) Stale() string {
	if c.git == nil || c.git.pulled == nil {
		return ""
	}
	return fmt.Sprintf("could not pull the catalog, so other machines' repos may be out of date: %v", c.git.pulled)
}

// Where is where this machine's list lives.
func (c *Collection) Where() string { return filepath.Dir(c.listPath()) }

// Save writes this machine's list atomically.
func (c *Collection) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(c.listPath(), append(b, '\n'))
}

// writeFile writes path atomically, making its directory.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var (
	nameOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hostOK = nameOK
)

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
	c.Repos = append(c.Repos, Repo{Name: name, Path: path, Out: filepath.Join("archives", name), Added: now.UTC()})
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
