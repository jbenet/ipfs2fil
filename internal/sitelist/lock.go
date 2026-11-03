package sitelist

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ipfs/go-cid"
)

// Lock is a list's lock file: what each site served when last looked up.
// Each run rewrites it, so committing it next to the list keeps a site's
// history in git as well.
type Lock struct {
	List    string    `toml:"list"`    // the list's file name
	Checked time.Time `toml:"checked"` // when the run that wrote it ended
	Site    []Entry   `toml:"site"`
}

// Entry is one site and the root it serves. It is a [[site]] table in the
// lock file, and an entry of sites.json in an ipfs2fil archive.
type Entry struct {
	Site
	Root string `json:"root,omitempty" toml:"root,omitempty"`
	// How names the steps that found Root, such as "dnslink" or
	// "ens via eth.limo, then ipns".
	How string `json:"how,omitempty" toml:"how,omitempty"`
	// Since is when Root was first found; Checked is when it was last
	// found again.
	Since   *time.Time `json:"since,omitempty" toml:"since,omitempty"`
	Checked *time.Time `json:"checked,omitempty" toml:"checked,omitempty"`
	// Stale says the site could not be looked up or fetched this time, so
	// Root is the one found last; Error says why.
	Stale bool   `json:"stale,omitempty" toml:"stale,omitempty"`
	Error string `json:"error,omitempty" toml:"error,omitempty"`
	// Earlier are the roots it served before, newest first.
	Earlier []Earlier `json:"earlier,omitempty" toml:"earlier,omitempty"`
}

// Earlier is a root a site served once.
type Earlier struct {
	Root  string    `json:"root" toml:"root"`
	Since time.Time `json:"since" toml:"since"`
	Until time.Time `json:"until" toml:"until"` // when a new root was found
}

// maxEarlier bounds each site's Earlier.
const maxEarlier = 100

// ReadLock reads a lock file. A missing one is an empty lock.
func ReadLock(path string) (*Lock, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Lock{}, nil
	}
	if err != nil {
		return nil, err
	}
	var l Lock
	if _, err := toml.Decode(string(b), &l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &l, nil
}

// Encode writes the lock file's bytes.
func (l *Lock) Encode() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# What each site in %s served when last looked up.\n# Written by ipfs-sites and ipfs2fil on each run; edits are overwritten.\n\n", l.List)
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(l); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Write writes the lock file at path, atomically.
func (l *Lock) Write(path string) error {
	b, err := l.Encode()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lock-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), path)
}

// find is the lock's entry for s: the same source, else the same name.
func (l *Lock) find(s Site) *Entry {
	for i := range l.Site {
		if l.Site[i].Source == s.Source {
			return &l.Site[i]
		}
	}
	for i := range l.Site {
		if l.Site[i].Name == s.Name {
			return &l.Site[i]
		}
	}
	return nil
}

// Options say how Update looks sites up and fetches them.
type Options struct {
	Resolver *Resolver
	// Fetch downloads the DAG under a root, and Have says whether it was
	// downloaded before. Without Fetch, sites are only looked up.
	Fetch func(ctx context.Context, root cid.Cid) error
	Have  func(root cid.Cid) bool
	// Workers is how many sites are looked up at once (default 4).
	Workers int
	Now     func() time.Time
	Log     io.Writer // one line per site, if set
}

// Update looks up every site of l now, fetching what each serves when
// opts.Fetch is set, and returns the new lock. A site that cannot be looked
// up or fetched keeps the root lock had for it (if fetched before, when
// fetching), marked Stale.
func Update(ctx context.Context, l *List, lock *Lock, opts Options) *Lock {
	if lock == nil {
		lock = &Lock{}
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 4
	}
	res := opts.Resolver
	if res == nil {
		res = &Resolver{}
	}
	out := &Lock{List: filepath.Base(l.Path), Site: make([]Entry, len(l.Sites))}
	var wg sync.WaitGroup
	var logMu sync.Mutex
	logf := func(format string, a ...any) {
		if opts.Log != nil {
			logMu.Lock()
			fmt.Fprintf(opts.Log, format, a...)
			logMu.Unlock()
		}
	}
	sem := make(chan struct{}, workers)
	for i, s := range l.Sites {
		var prev Entry
		if p := lock.find(s); p != nil {
			prev = *p
		}
		out.Site[i].Site = s
		wg.Add(1)
		go func(e *Entry, prev Entry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.Since, e.Checked, e.Earlier = prev.Since, prev.Checked, prev.Earlier
			got, err := res.Resolve(ctx, e.Site)
			if err == nil && opts.Fetch != nil {
				if opts.Have == nil || !opts.Have(got.Root) {
					logf("  %s: fetching %s\n", e.Name, got.Root)
				}
				err = opts.Fetch(ctx, got.Root)
			}
			if err == nil {
				t := now().UTC().Truncate(time.Second)
				e.Root, e.How, e.Checked = got.Root.String(), got.How, &t
				if prev.Root != e.Root {
					if prev.Root != "" {
						since := t
						if prev.Since != nil {
							since = *prev.Since
						}
						e.Earlier = append([]Earlier{{Root: prev.Root, Since: since, Until: t}}, prev.Earlier...)
						if len(e.Earlier) > maxEarlier {
							e.Earlier = e.Earlier[:maxEarlier]
						}
					}
					e.Since = &t
				}
				logf("  %s: %s (%s)\n", e.Name, e.Root, e.How)
				return
			}
			e.Error = err.Error()
			if c, perr := cid.Decode(prev.Root); perr == nil && (opts.Fetch == nil || opts.Have == nil || opts.Have(c)) {
				e.Root, e.How, e.Stale = prev.Root, prev.How, true
				logf("  %s: %v; keeping the root found last, %s\n", e.Name, err, prev.Root)
				return
			}
			e.Since, e.Checked = nil, nil
			logf("  %s: %v\n", e.Name, err)
		}(&out.Site[i], prev)
	}
	wg.Wait()
	out.Checked = now().UTC().Truncate(time.Second)
	return out
}
