package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/feedback"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

type exploreOpts struct {
	serve    bool
	addr     string
	demo     bool
	scale    int
	feedback bool
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
refreshes the pages. It also serves the files themselves, read from the
pieces on this machine, so the explorer can show photos, play videos and open
documents, and it serves this version's explorer pages over the ones packed
with the archive.

It finds what 'ipfs2fil up' or 'pack' wrote: the archive in --out if given or
if ./ipfs2fil-archive holds one, else the repos 'ipfs2fil repos add' listed in
--home. Give a repo's name (or --repo with its path) to explore just that one;
otherwise every packed repo is served, with a page listing them.

--demo writes a made-up archive and serves it.

--feedback is for developing the explorer: it adds a feedback button (Alt+F,
Option+F on a Mac) to every page served, with screenshots, region capture and
annotation (github.com/jbenet/feedback-dev-kit). Reports are saved as issues
in feedback/issues/NNNN-slug.md under --home (~/.ipfs2fil), where a coding
agent can pick them up, and listed at /_i2f/feedback/issues. Only this local
server adds it: archives and gateways never show it.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.repo, "repo", "", "with no --out, the listed repo to explore, by path or name")
			outFlag(fs, o)
			fs.StringVar(&o.out, "archive", o.out, "same as --out")
			homeFlag(fs, o)
			fs.BoolVar(&exploreFlags.serve, "serve", true, "serve the explorer on --addr; false only refreshes it")
			fs.StringVar(&exploreFlags.addr, "addr", "127.0.0.1:8484", "address for --serve")
			fs.BoolVar(&exploreFlags.demo, "demo", false, "write a made-up archive into --out (a new temp dir if unset) and serve it")
			fs.IntVar(&exploreFlags.scale, "scale", 1, "with --demo, how big the made-up archive is")
			fs.BoolVar(&exploreFlags.feedback, "feedback", false, "developer mode: add a feedback button to the pages, saving reports under --home/feedback")
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
	var more []string
	var wraps []explorer.Wrap
	if e.feedback && e.serve {
		w, err := feedbackMode(ctx, o)
		if err != nil {
			return err
		}
		wraps = append(wraps, w)
	}
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
			}, wraps...)
		}
		dir = archives[0].Dir
		// The one repo asked for can share blocks with the other listed repos.
		if o.repo != "" {
			all := *o
			all.repo = ""
			if as, err := exploreArchives(&all); err == nil {
				for _, a := range as {
					if a.Dir != dir {
						more = append(more, a.Dir)
					}
				}
			}
		}
	}
	if !e.serve {
		return nil
	}
	return explorer.Serve(ctx, dir, more, e.addr, func(url string) {
		fmt.Fprintf(o.stdout, "serving %s at %s (ctrl-c stops)\n", dir, url)
	}, wraps...)
}

// feedbackMode starts filing feedback into --home/feedback and returns what
// adds the feedback kit to the served pages.
func feedbackMode(ctx context.Context, o *opts) (explorer.Wrap, error) {
	home := o.home
	if home == "" {
		d, err := collection.DefaultDir()
		if err != nil {
			return nil, err
		}
		home = d
	}
	who := "me"
	if u, err := user.Current(); err == nil && u.Username != "" {
		who = u.Username
	}
	fb, err := feedback.New(filepath.Join(home, "feedback"), who, ctx.Done())
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(o.stdout, "feedback mode: Alt+F (Option+F on a Mac) on any page; reports go to %s, listed at %s/issues\n",
		filepath.Join(fb.Dir, "issues"), feedback.Base)
	return func(mux *http.ServeMux, h http.Handler) http.Handler {
		fb.Handle(mux)
		return feedback.Inject(h)
	}, nil
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
