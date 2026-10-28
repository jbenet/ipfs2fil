package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

type exploreOpts struct {
	serve bool
	addr  string
	demo  bool
	scale int
}

var exploreFlags exploreOpts

func init() {
	register(&command{
		name:    "explore",
		summary: "Write or serve the HTML explorer",
		help: `
  ipfs2fil explore [name]

Serves the archive's HTML explorer on --addr, after refreshing explorer/ from
the manifest so it shows the latest copies and proofs. The same pages work
from any IPFS gateway once the archive is stored. --serve=false only
refreshes the pages.

It finds what 'ipfs2fil up' or 'pack' wrote: the archive in --out if given or
if ./ipfs2fil-archive holds one, else the repos 'ipfs2fil repos add' listed in
--home. Give a repo's name (or --repo with its path) to explore just that one;
otherwise every packed repo is served, with a page listing them.

--demo writes a made-up archive and serves it.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.repo, "repo", "", "with no --out, the listed repo to explore, by path or name")
			outFlag(fs, o)
			fs.StringVar(&o.out, "archive", o.out, "same as --out")
			homeFlag(fs, o)
			fs.BoolVar(&exploreFlags.serve, "serve", true, "serve the explorer on --addr; false only refreshes it")
			fs.StringVar(&exploreFlags.addr, "addr", "127.0.0.1:8484", "address for --serve")
			fs.BoolVar(&exploreFlags.demo, "demo", false, "write a made-up archive into --out (a new temp dir if unset) and serve it")
			fs.IntVar(&exploreFlags.scale, "scale", 1, "with --demo, how big the made-up archive is")
		},
		run: func(ctx context.Context, o *opts, args []string) error {
			if len(args) > 1 {
				return errors.New("explore takes at most one repo name")
			}
			if len(args) == 1 {
				o.repo = args[0]
			}
			return runExplore(ctx, o, exploreFlags)
		},
	})
}

func runExplore(ctx context.Context, o *opts, e exploreOpts) error {
	dir := o.out
	if e.demo {
		if !o.set["out"] && !o.set["archive"] {
			d, err := os.MkdirTemp("", "ipfs2fil-demo-")
			if err != nil {
				return err
			}
			dir = d
		}
		m, entries := explorer.Demo(e.scale)
		if err := os.MkdirAll(filepath.Join(dir, "index"), 0o755); err != nil {
			return err
		}
		if err := m.Write(filepath.Join(dir, "index", "manifest.json")); err != nil {
			return err
		}
		if err := explorer.Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
			return err
		}
		fmt.Fprintf(o.stdout, "wrote a demo archive explorer to %s (%d entries)\n", dir, len(entries))
		e.serve = true
	} else {
		archives, err := exploreArchives(o)
		if err != nil {
			return err
		}
		for _, a := range archives {
			if err := refreshExplorer(a.Dir); err != nil {
				return err
			}
			fmt.Fprintf(o.stdout, "explorer in %s is up to date\n", filepath.Join(a.Dir, "explorer"))
		}
		if len(archives) > 1 {
			if !e.serve {
				return nil
			}
			return explorer.ServeMany(ctx, archives, e.addr, func(url string) {
				fmt.Fprintf(o.stdout, "serving %d archives at %s (ctrl-c stops)\n", len(archives), url)
			})
		}
		dir = archives[0].Dir
	}
	if !e.serve {
		return nil
	}
	return explorer.Serve(ctx, dir, e.addr, func(url string) {
		fmt.Fprintf(o.stdout, "serving %s at %s (ctrl-c stops)\n", dir, url)
	})
}

// hasArchive says whether dir holds a packed archive.
func hasArchive(dir string) bool {
	for _, p := range []string{manifestPath(dir), filepath.Join(dir, "index", "manifest.json")} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// exploreArchives finds what to explore: --out when given or when it holds
// an archive (the default ./ipfs2fil-archive of a plain 'pack'), else the
// listed repos 'up --all' packed into --home, or the one --repo names.
func exploreArchives(o *opts) ([]explorer.Archive, error) {
	explicit := o.set["out"] || o.set["archive"]
	if explicit || (hasArchive(o.out) && o.repo == "") {
		if !hasArchive(o.out) {
			return nil, fmt.Errorf("no archive in %s; run 'ipfs2fil pack' first, or pass --out with the directory pack wrote", o.out)
		}
		return []explorer.Archive{{Name: filepath.Base(o.out), Dir: o.out}}, nil
	}
	c, err := openCollection(o)
	if err != nil {
		return nil, err
	}
	var as []explorer.Archive
	var named []string
	for _, r := range c.Repos {
		named = append(named, r.Name)
		if o.repo != "" && o.repo != r.Name && !samePath(o.repo, r.Path) {
			continue
		}
		dir := c.OutDir(r)
		if !hasArchive(dir) {
			if o.repo != "" {
				return nil, fmt.Errorf("%s is not packed yet; run 'ipfs2fil up --all' (or 'pack --all') first", r.Name)
			}
			continue
		}
		as = append(as, explorer.Archive{Name: r.Name, Dir: dir, Repo: r.Path})
	}
	switch {
	case len(as) > 0:
		return as, nil
	case o.repo != "" && len(named) > 0:
		return nil, fmt.Errorf("no repo %q in the list in %s; the listed repos are %s", o.repo, c.Where(), strings.Join(named, ", "))
	case len(named) > 0:
		return nil, fmt.Errorf("none of the repos listed in %s is packed yet; run 'ipfs2fil up --all' first", c.Where())
	}
	return nil, fmt.Errorf("no archive in %s and no repos listed in %s; run 'ipfs2fil up' (or 'pack') first, or pass --out with the directory it wrote", o.out, c.Where())
}

// samePath says whether a and b name the same directory.
func samePath(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if a == b {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// refreshExplorer rewrites dir's explorer data from its manifest. The full
// manifest pack and push keep in dir has the copies; an archive fetched from
// IPFS has only index/manifest.json.
func refreshExplorer(dir string) error {
	m, err := manifest.Read(manifestPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		m, err = manifest.Read(filepath.Join(dir, "index", "manifest.json"))
	}
	if err != nil {
		return err
	}
	return explorer.WriteManifest(filepath.Join(dir, "explorer"), m)
}
