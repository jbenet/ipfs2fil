package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

// opts holds the flags commands share. Each command registers the ones it uses.
type opts struct {
	stdout, stderr io.Writer
	set            map[string]bool // flags given on the command line

	repo   string
	out    string
	copies int
	json   bool
	yes    bool
}

func repoFlag(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.repo, "repo", "", "the IPFS repo to read (default $IPFS_PATH, else ~/.ipfs)")
}

func outFlag(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.out, "out", "ipfs2fil-archive", "directory for the archive, its pieces and resume state")
}

func copiesFlag(fs *flag.FlagSet, o *opts) {
	fs.IntVar(&o.copies, "copies", 3, "how many providers store a copy")
}

func jsonFlag(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.json, "json", false, "print JSON instead of text")
}

func yesFlag(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (for scripts)")
}

// openRepo finds and opens the repo read-only, with errors that say what to do.
func openRepo(o *opts) (repo.Repo, error) {
	path, err := repo.Discover(o.repo)
	if err != nil {
		switch {
		case o.repo != "" && !errors.Is(err, repo.ErrNotRepo):
			return nil, fmt.Errorf("reading --repo %s: %w", o.repo, err)
		case o.repo != "":
			return nil, fmt.Errorf("%s is not an IPFS repo; point --repo at the directory holding 'config' and 'datastore' or 'blocks'", o.repo)
		case errors.Is(err, repo.ErrNotRepo):
			return nil, errors.New("no IPFS repo found in $IPFS_PATH, ~/.ipfs or ~/.go-ipfs; pass --repo <path>")
		}
		return nil, err
	}
	r, err := repo.Open(path)
	if errors.Is(err, repo.ErrUnsupported) {
		return nil, fmt.Errorf("%s: %w; please file an issue at https://github.com/jbenet/ipfs2fil/issues with the repo's 'version' file", path, err)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return r, nil
}
