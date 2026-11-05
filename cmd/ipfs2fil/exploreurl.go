package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

var exploreURLGateway string

func init() {
	register(&command{
		name:    "explore-url",
		summary: "Print the IPFS URL of the explorer stored with an archive",
		help: `
  ipfs2fil explore-url [name]

Prints where to open the explorer that is stored on Filecoin with the
archive: <gateway>/ipfs/<root>/explorer/. It works from any browser once the
pieces are stored and the providers have announced them, with no ipfs2fil
and no local copy.

It finds archives the way 'ipfs2fil explore' does: --out if given or if
./ipfs2fil-archive holds one, else every repo 'ipfs2fil repos add' listed in
--home, one line each, or just the one a name (or --repo) picks. With one
archive it prints the URL alone, so it can go straight to a browser:

  open "$(ipfs2fil explore-url)"

Each URL is the latest snapshot's root. An archive with no copies yet is
marked; run 'ipfs2fil up' (or push) to store it.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.repo, "repo", "", "with no --out, the listed repo, by path or name")
			outFlag(fs, o)
			fs.StringVar(&o.out, "archive", o.out, "same as --out")
			homeFlag(fs, o)
			fs.StringVar(&exploreURLGateway, "gateway", "https://dweb.link", "IPFS gateway to put in the URL")
		},
		run: func(ctx context.Context, o *opts, args []string) error {
			if len(args) > 1 {
				return errors.New("explore-url takes at most one repo name")
			}
			if len(args) == 1 {
				o.repo = args[0]
			}
			return runExploreURL(o, exploreURLGateway)
		},
	})
}

// explorerURL is where a gateway serves the explorer under an archive root.
func explorerURL(gateway, root string) string {
	return strings.TrimRight(gateway, "/") + "/ipfs/" + root + "/explorer/"
}

func runExploreURL(o *opts, gateway string) error {
	as, err := exploreArchives(o)
	if err != nil {
		return err
	}
	type line struct{ name, url, note string }
	var ls []line
	for _, a := range as {
		m, err := archiveManifest(a.Dir)
		if err != nil {
			return err
		}
		if m.Root == "" {
			return fmt.Errorf("the archive in %s has no root yet; run 'ipfs2fil pack' again", a.Dir)
		}
		l := line{name: a.Name, url: explorerURL(gateway, m.Root)}
		if len(m.Copies) == 0 {
			l.note = "not stored yet: run 'ipfs2fil up' first"
		}
		ls = append(ls, l)
	}
	if len(ls) == 1 {
		fmt.Fprintln(o.stdout, ls[0].url)
		if ls[0].note != "" {
			fmt.Fprintf(o.stderr, "ipfs2fil: %s is %s\n", ls[0].name, ls[0].note)
		}
		return nil
	}
	w := tabwriter.NewWriter(o.stdout, 0, 0, 2, ' ', 0)
	for _, l := range ls {
		if l.note != "" {
			l.note = "  (" + l.note + ")"
		}
		fmt.Fprintf(w, "%s\t%s%s\n", l.name, l.url, l.note)
	}
	return w.Flush()
}

// archiveManifest reads an archive's manifest: the one pack keeps beside the
// pieces, else the copy inside the archive's index/.
func archiveManifest(dir string) (*manifest.Manifest, error) {
	m, err := manifest.Read(manifestPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		m, err = manifest.Read(filepath.Join(dir, "index", "manifest.json"))
	}
	return m, err
}
