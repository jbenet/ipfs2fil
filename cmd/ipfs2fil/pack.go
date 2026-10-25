package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

var packOrphans bool

func init() {
	register(&command{
		name:    "pack",
		summary: "Build the archive and its pieces on disk",
		help: `
Reads the repo and writes the archive into --out: pieces/ (one CAR per piece,
named by PieceCID), index/, explorer/ and manifest.json. The repo is only
read, and pack is safe to run while the daemon runs.

Pieces are cut where the content says, so packing the same repo again gives
the same pieces, and after an edit only nearby pieces change. Run pack again
after the repo changes, then push: only the new pieces upload. A pack stopped
with Ctrl-C can simply be run again.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoFlag(fs, o)
			outFlag(fs, o)
			fs.BoolVar(&packOrphans, "orphans", true, "include blocks no pin or MFS reaches, under data/orphans")
		},
		run: func(ctx context.Context, o *opts, _ []string) error {
			_, err := pack(ctx, o)
			return err
		},
	})
}

func pack(ctx context.Context, o *opts) (*archive.Result, error) {
	r, err := openRepo(o)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return nil, fmt.Errorf("making --out %s: %w", o.out, err)
	}
	opt := archive.DefaultOptions()
	opt.Orphans = packOrphans
	opt.Tool = "ipfs2fil " + version
	var bar *progress
	stage := ""
	opt.Progress = func(st string, done, total int64) {
		if st != stage {
			if bar != nil {
				bar.Done()
			}
			label := map[string]string{"walk": "reading blocks", "write": "writing pieces"}[st]
			if label == "" {
				label = st
			}
			bar, stage = newProgress(o.stderr, label, total, st == "write"), st
		}
		bar.Set(done, total)
	}
	before := map[string]bool{}
	if prev, err := manifest.Read(manifestPath(o.out)); err == nil {
		for _, p := range prev.Pieces {
			before[p.PieceCID] = true
		}
	}
	fmt.Fprintf(o.stderr, "packing %s (fs-repo %d) into %s\n", r.Path(), r.Version(), o.out)
	res, err := archive.Build(ctx, r, o.out, opt)
	if bar != nil {
		bar.Done()
	}
	if err != nil {
		return nil, err
	}
	m := res.Manifest
	var bytes int64
	for _, p := range m.Pieces {
		bytes += p.CARBytes
	}
	fresh := 0
	for _, p := range m.Pieces {
		if !before[p.PieceCID] {
			fresh++
		}
	}
	w := o.stdout
	fmt.Fprintf(w, "archive  %s\n", m.Root)
	fmt.Fprintf(w, "roots    %d (pins, MFS and orphans)\n", len(m.Roots))
	if len(before) > 0 {
		fmt.Fprintf(w, "pieces   %d, %s (%d changed since the last pack)\n", len(m.Pieces), humanBytes(bytes), fresh)
	} else {
		fmt.Fprintf(w, "pieces   %d, %s\n", len(m.Pieces), humanBytes(bytes))
	}
	if len(res.Missing) > 0 {
		fmt.Fprintf(w, "missing  %d blocks the pins link to were never in the repo; the archive has everything else\n", len(res.Missing))
	}
	if isDir(filepath.Join(o.out, "explorer")) {
		fmt.Fprintf(w, "explore  ipfs2fil explore --serve --out %s\n", o.out)
	}
	fmt.Fprintf(w, "next     ipfs2fil push --out %s\n", o.out)
	return res, nil
}
