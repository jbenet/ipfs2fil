package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/collection"
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

	// --all runs over every repo in the collection in --home (repos.go).
	all     bool
	home    string
	catalog string
	host    string
	// Set by pack --all for each repo: blocks earlier repos store, and the
	// collection's data set key.
	shared        func(multihash.Multihash) (archive.SharedBlock, bool)
	collectionKey string
	// record is the sealed catalog record pack --all puts in each archive
	// when the catalog is read from chain.
	record []byte
}

func repoFlag(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.repo, "repo", "", "the IPFS repo to read (default $IPFS_PATH, else ~/.ipfs)")
}

// repoOnlyFlag accepts --repo on commands that work from --out alone, so the
// same flags work for every step.
func repoOnlyFlag(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.repo, "repo", "", "accepted for symmetry with pack; this command reads the archive in --out")
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

func homeFlag(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.home, "home", "", "directory holding the list of repos and their archives (default $"+collection.EnvHome+", else ~/.ipfs2fil)")
	fs.StringVar(&o.catalog, "catalog", "", "where repos on all your machines are tracked: filecoin (read from your data sets on chain), a git repository URL, or a shared folder; remembered once given (default $"+collection.EnvCatalog+", else filecoin)")
	fs.StringVar(&o.host, "host", "", "this machine's name in the catalog (default $"+collection.EnvHost+", else the host name)")
}

// allFlag adds --all and --home: run over every repo 'ipfs2fil repos add' listed.
func allFlag(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.all, "all", false, "every repo in the list 'ipfs2fil repos add' keeps, instead of --repo and --out")
	homeFlag(fs, o)
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
