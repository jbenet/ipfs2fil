package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
)

var exportTo string

func init() {
	register(&command{
		name:    "export",
		summary: "Write a repo back out from its archive",
		help: `
Rebuilds the repo an archive came from into --to, from the archive's pieces
alone: the CARs pack wrote, and for pieces not on disk any more, the copies
stored on Filecoin (read back from a provider; no wallet key needed,
--address is enough).

What comes back:
  CAR files     the very files, byte for byte (export.json says so per file)
  Bluesky       the account as ipfs2fil fetched it: repo.car, byte for byte,
                blobs/ and the identity documents
  other repos   blocks.car with every block, pins.json with every pin and its
                name, export.json with the MFS root, and import-to-kubo.sh,
                which loads all of it into a kubo repo

Archives packed before this version have no record of their CAR files'
layout, so their CAR repos come back as blocks.car; pack once more with
--new-pages to add it. A kubo repo's config and keys are never archived.

It exports the archive in --out if given or if ./ipfs2fil-archive holds one,
else the listed repo --repo names. --repo host/name exports a repo another
of your machines stored, read back from Filecoin.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.repo, "repo", "", "with no --out, the listed repo to export, by name or path")
			outFlag(fs, o)
			fs.StringVar(&exportTo, "to", "", "the directory to write the repo into; it must be empty or not exist")
			homeFlag(fs, o)
			addReadStoreFlags(fs)
		},
		run: runExport,
	})
}

func runExport(ctx context.Context, o *opts, _ []string) error {
	if exportTo == "" {
		return errors.New("say where to write the repo with --to DIR")
	}
	if ents, err := os.ReadDir(exportTo); err == nil && len(ents) > 0 {
		return fmt.Errorf("%s is not empty; export into a new directory", exportTo)
	}
	archives, err := exploreArchives(o)
	if err != nil && strings.Contains(o.repo, "/") {
		// Another machine's repo, as host/name: its manifest and block
		// lists come from the catalog, its pieces from Filecoin.
		var other error
		if archives, other = otherArchive(ctx, o); other != nil {
			return errors.Join(err, other)
		}
		err = nil
	}
	if err != nil {
		return err
	}
	if len(archives) > 1 {
		var names []string
		for _, a := range archives {
			names = append(names, a.Name)
		}
		return fmt.Errorf("%d repos are listed (%s); pick one with --repo", len(archives), strings.Join(names, ", "))
	}
	a := archives[0]
	m, err := manifest.Read(manifestPath(a.Dir))
	if err != nil {
		return err
	}
	// Blocks shared between repos sit in other listed repos' pieces.
	var dirs []string
	if others, err := listedArchives(&opts{stdout: io.Discard, stderr: io.Discard, home: o.home, catalog: o.catalog, host: o.host, set: map[string]bool{}}); err == nil {
		for _, x := range others {
			if !samePath(x.Dir, a.Dir) {
				dirs = append(dirs, x.Dir)
			}
		}
	}

	var s store.Store
	var closer io.Closer
	defer func() {
		if closer != nil {
			closer.Close()
		}
	}()
	fetch := func(ctx context.Context, pieceCID string) (io.ReadCloser, error) {
		if len(m.Copies) == 0 {
			return nil, errors.New("the archive was never pushed, and its CAR is gone")
		}
		if s == nil {
			if s, closer, err = openStore(ctx, o); err != nil {
				return nil, err
			}
		}
		var errs []error
		for _, c := range m.Copies {
			rc, err := s.Fetch(ctx, store.DataSet{Provider: c.Provider, ID: c.DataSetID}, pieceCID)
			if err == nil {
				return rc, nil
			}
			errs = append(errs, fmt.Errorf("provider %s: %w", c.Provider, err))
		}
		return nil, errors.Join(errs...)
	}

	var bar *progress
	stage := ""
	fmt.Fprintf(o.stderr, "exporting %s into %s\n", a.Name, exportTo)
	res, err := archive.Export(ctx, a.Dir, exportTo, archive.ExportOptions{Dirs: dirs, Fetch: fetch,
		Progress: func(st string, done, total int64) {
			if st != stage {
				if bar != nil {
					bar.Done()
				}
				label := map[string]string{"fetch": "reading pieces back", "write": "writing blocks"}[st]
				bar, stage = newProgress(o.stderr, label, total, false), st
			}
			bar.Set(done, total)
		}})
	if bar != nil {
		bar.Done()
	}
	if err != nil {
		return err
	}
	w := o.stdout
	fmt.Fprintf(w, "exported %s (%s): %d blocks, %s\n", a.Name, res.Kind, res.Blocks, humanBytes(res.Bytes))
	same, differ := 0, 0
	for _, f := range res.Files {
		if f.Same == nil {
			continue
		}
		if *f.Same {
			same++
		} else {
			differ++
			fmt.Fprintf(w, "differs  %s is not byte for byte the file the repo had\n", f.Path)
		}
	}
	if same > 0 {
		fmt.Fprintf(w, "same     %d files are byte for byte the ones the repo had\n", same)
	}
	if len(res.Pins) > 0 || res.MFS != "" {
		fmt.Fprintf(w, "pins     %d", len(res.Pins))
		if res.MFS != "" {
			fmt.Fprintf(w, ", MFS root %s", res.MFS)
		}
		fmt.Fprintln(w)
	}
	if len(res.Missing) > 0 {
		fmt.Fprintf(w, "missing  %d blocks the repo never had when it was packed\n", len(res.Missing))
	}
	if _, err := os.Stat(filepath.Join(exportTo, archive.ExportImport)); err == nil {
		fmt.Fprintf(w, "next     sh %s  (loads it into the kubo repo at $IPFS_PATH)\n", filepath.Join(exportTo, archive.ExportImport))
	}
	if differ > 0 {
		return fmt.Errorf("%d files differ from the originals", differ)
	}
	return nil
}

// otherArchive finds the repo --repo names as host/name among other
// machines' repos in the catalog, read from chain first if that is where it
// is kept.
func otherArchive(ctx context.Context, o *opts) ([]explorer.Archive, error) {
	c, err := openCollection(o)
	if err != nil {
		return nil, err
	}
	if c.OnChain() {
		s, closer, err := openStore(ctx, o)
		if err != nil {
			return nil, err
		}
		readCatalog(ctx, o, c, s)
		closer.Close()
	}
	es, err := c.Entries()
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !e.Local && e.ID() == o.repo {
			if m, err := e.Manifest(); err != nil || m == nil {
				return nil, fmt.Errorf("%s: its manifest is not in the catalog yet (%v)", o.repo, err)
			}
			return []explorer.Archive{{Name: e.ID(), Dir: e.Dir}}, nil
		}
	}
	return nil, fmt.Errorf("no repo %s in the catalog", o.repo)
}
