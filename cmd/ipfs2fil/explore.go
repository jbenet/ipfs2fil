package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

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
Refreshes explorer/ in the archive directory from index/manifest.json, so the
explorer shows the latest copies and proofs. The same pages work from any IPFS
gateway once the archive is stored.

--serve serves the archive directory on --addr so you can browse it before (or
after) pushing. --demo writes a made-up archive and serves it.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			fs.StringVar(&o.out, "archive", o.out, "same as --out")
			fs.BoolVar(&exploreFlags.serve, "serve", false, "serve the archive and its explorer on --addr")
			fs.StringVar(&exploreFlags.addr, "addr", "127.0.0.1:8484", "address for --serve")
			fs.BoolVar(&exploreFlags.demo, "demo", false, "write a made-up archive into --out (a new temp dir if unset) and serve it")
			fs.IntVar(&exploreFlags.scale, "scale", 1, "with --demo, how big the made-up archive is")
		},
		run: func(ctx context.Context, o *opts, _ []string) error { return runExplore(ctx, o, exploreFlags) },
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
		// The full manifest pack and push keep in dir has the copies; an
		// archive fetched from IPFS has only index/manifest.json.
		m, err := manifest.Read(manifestPath(dir))
		if errors.Is(err, os.ErrNotExist) {
			m, err = manifest.Read(filepath.Join(dir, "index", "manifest.json"))
		}
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no archive in %s; run 'ipfs2fil pack' first, or pass --out with the directory pack wrote", dir)
		}
		if err != nil {
			return err
		}
		if err := explorer.WriteManifest(filepath.Join(dir, "explorer"), m); err != nil {
			return err
		}
		fmt.Fprintf(o.stdout, "explorer in %s is up to date\n", filepath.Join(dir, "explorer"))
	}
	if !e.serve {
		return nil
	}
	return explorer.Serve(ctx, dir, e.addr, func(url string) {
		fmt.Fprintf(o.stdout, "serving %s at %s (ctrl-c stops)\n", dir, url)
	})
}
