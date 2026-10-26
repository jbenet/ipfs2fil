package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

var (
	reposName  string
	reposForce bool
)

func init() {
	register(&command{
		name:    "repos",
		summary: "Keep a list of repos to archive together (add, ls, rm)",
		help: `
  ipfs2fil repos add <path> [--name <name>]
  ipfs2fil repos ls [--json]
  ipfs2fil repos rm <name> [--force]

Keeps a list of IPFS repos in --home (default ~/.ipfs2fil), each with its
archive under archives/<name>. Then 'ipfs2fil up --all' packs and pushes them
all, and 'ipfs2fil status --all' shows them all with their costs.

Repos in the list share one set of data sets (one per copy) instead of a set
each, so setup is paid once, and a block an earlier repo already stores is not
stored again by a later one. The order repos were added in matters: later
repos may rely on earlier repos' pieces, never the other way round.

The list holds repo paths, so it stays on this machine; nothing in it is
uploaded. rm only takes a repo off the list: its archive stays on disk and its
pieces stay stored.

Repos on several machines: run ipfs2fil on each machine with the same wallet
key and the same --catalog (or $IPFS2FIL_CATALOG), a directory every machine
sees, such as a synced folder or a git checkout. Each machine keeps its list,
manifests and block lists there under its host name (--host); pieces stay on
the machine that packed them. Then 'up --all' on a machine packs and pushes
its own repos, leaving out blocks other machines' repos already store, and
'status --all' on any machine shows every repo on every machine.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.repo, "repo", "", "for add: the IPFS repo to add (or give it as the argument)")
			fs.StringVar(&reposName, "name", "", "for add: the name to list it under (default: the repo directory's name)")
			fs.BoolVar(&reposForce, "force", false, "for rm: remove a repo even if later repos rely on its pieces")
			homeFlag(fs, o)
			jsonFlag(fs, o)
		},
		run: runRepos,
	})
}

func runRepos(ctx context.Context, o *opts, args []string) error {
	if len(args) == 0 {
		return errors.New("say what to do: 'ipfs2fil repos add <path>', 'repos ls' or 'repos rm <name>'")
	}
	sub, rest, err := reposArgs(o, args[0], args[1:])
	if err != nil {
		return err
	}
	c, err := openCollection(o)
	if err != nil {
		return err
	}
	switch sub {
	case "add":
		return reposAdd(o, c, rest)
	case "ls", "list":
		return reposList(o, c)
	case "rm", "remove":
		return reposRemove(o, c, rest)
	}
	return fmt.Errorf("no 'repos %s'; use add, ls or rm", sub)
}

// reposArgs parses flags that come after the subcommand, between or after
// its arguments: 'repos add ~/x --name x'.
func reposArgs(o *opts, sub string, args []string) (string, []string, error) {
	fs := flag.NewFlagSet("repos "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Registering a flag resets it to its default, so keep what was given
	// before the subcommand.
	saved, name, force := *o, reposName, reposForce
	commands["repos"].flags(fs, o)
	*o, reposName, reposForce = saved, name, force
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", nil, fmt.Errorf("ipfs2fil repos %s: %v; run 'ipfs2fil help repos'", sub, err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return sub, rest, nil
		}
		rest, args = append(rest, args[0]), args[1:]
	}
}

func openCollection(o *opts) (*collection.Collection, error) {
	dir := o.home
	if dir == "" {
		d, err := collection.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	catalog, host := o.catalog, o.host
	if catalog == "" {
		catalog = os.Getenv(collection.EnvCatalog)
	}
	if host == "" {
		host = os.Getenv(collection.EnvHost)
	}
	if host == "" {
		host = collection.HostName()
	}
	return collection.OpenShared(dir, catalog, host)
}

func reposAdd(o *opts, c *collection.Collection, args []string) error {
	path := o.repo
	switch {
	case len(args) == 1 && path == "":
		path = args[0]
	case len(args) > 1 || (len(args) == 1 && path != ""):
		return errors.New("add one repo at a time: 'ipfs2fil repos add <path>'")
	case path == "":
		return errors.New("say which repo: 'ipfs2fil repos add <path>'")
	}
	found, err := repo.Discover(path)
	if errors.Is(err, repo.ErrNotRepo) {
		return fmt.Errorf("%s is not an IPFS repo; give the directory holding 'config' and 'datastore' or 'blocks'", path)
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	abs, err := filepath.Abs(found)
	if err != nil {
		return err
	}
	r, err := repo.Open(abs)
	if err != nil {
		return fmt.Errorf("opening %s: %w", abs, err)
	}
	v := r.Version()
	r.Close()
	name := reposName
	if name == "" {
		name = defaultName(abs)
	}
	if _, err := c.Add(name, abs, time.Now()); err != nil {
		return err
	}
	if err := c.Save(); err != nil {
		return err
	}
	fmt.Fprintf(o.stdout, "added    %s (%s, fs-repo %d)\n", name, abs, v)
	fmt.Fprintf(o.stdout, "repos    %d on this machine, listed in %s\n", len(c.Repos), c.Where())
	fmt.Fprintf(o.stdout, "next     ipfs2fil up --all\n")
	return nil
}

// defaultName is the repo directory's name without leading dots, or its
// parent's when that leaves nothing useful (~/.ipfs -> ipfs).
func defaultName(path string) string {
	n := strings.TrimLeft(filepath.Base(path), ".")
	if n == "" {
		n = strings.TrimLeft(filepath.Base(filepath.Dir(path)), ".")
	}
	var b strings.Builder
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "repo"
	}
	return b.String()
}

type repoRow struct {
	Name         string `json:"name"` // host/name with a catalog
	Path         string `json:"path"`
	Local        bool   `json:"local"` // on this machine
	Out          string `json:"out,omitempty"`
	Archive      string `json:"archive,omitempty"` // latest archive root
	Pieces       int    `json:"pieces"`
	Bytes        int64  `json:"bytes"`
	SharedBlocks int64  `json:"sharedBlocks,omitempty"`
	SharedBytes  int64  `json:"sharedBytes,omitempty"`
	Snapshots    int    `json:"snapshots"`
	OwnDataSets  bool   `json:"ownDataSets,omitempty"` // pushed before it joined the list; keeps its own data sets
}

func repoRows(c *collection.Collection, es []collection.Entry) ([]repoRow, error) {
	rows := []repoRow{}
	for _, e := range es {
		row := repoRow{Name: e.ID(), Path: e.Path, Local: e.Local}
		if e.Local {
			row.Out = e.Dir
		}
		m, err := e.Manifest()
		if err != nil {
			return nil, fmt.Errorf("repo %s: %w", e.ID(), err)
		}
		if m != nil {
			row.Archive, row.Pieces, row.Snapshots = m.Root, len(m.Pieces), len(m.Snapshots)
			for _, p := range m.Pieces {
				row.Bytes += p.CARBytes
			}
			if m.Shared != nil {
				row.SharedBlocks, row.SharedBytes = m.Shared.Blocks, m.Shared.Bytes
			}
			row.OwnDataSets = m.Collection != c.Key
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func reposList(o *opts, c *collection.Collection) error {
	es, err := c.Entries()
	if err != nil {
		return err
	}
	rows, err := repoRows(c, es)
	if err != nil {
		return err
	}
	if o.json {
		e := json.NewEncoder(o.stdout)
		e.SetIndent("", "  ")
		return e.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintf(o.stdout, "no repos in %s yet; add one with 'ipfs2fil repos add <path>'\n", c.Where())
		return nil
	}
	fmt.Fprintf(o.stdout, "%d repos (%d on this machine)\n\n", len(rows), len(c.Repos))
	tw := tabwriter.NewWriter(o.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPIECES\tSIZE\tSHARED\tPATH")
	for _, r := range rows {
		pieces, size, shared := "not packed", "", ""
		if r.Archive != "" {
			pieces, size = fmt.Sprint(r.Pieces), humanBytes(r.Bytes)
		}
		if r.SharedBytes > 0 {
			shared = humanBytes(r.SharedBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, pieces, size, shared, r.Path)
	}
	tw.Flush()
	fmt.Fprintln(o.stdout, "\nSHARED is what earlier repos already store for it. 'ipfs2fil status --all' shows copies and costs.")
	return nil
}

func reposRemove(o *opts, c *collection.Collection, args []string) error {
	if len(args) != 1 {
		return errors.New("say which repo: 'ipfs2fil repos rm <name>' ('ipfs2fil repos ls' lists them)")
	}
	name := args[0]
	i := c.Find(name)
	if i < 0 {
		return fmt.Errorf("no repo named %q; 'ipfs2fil repos ls' lists them", name)
	}
	deps, err := c.Dependents(name)
	if err != nil {
		return err
	}
	if len(deps) > 0 && !reposForce {
		return fmt.Errorf("%s holds blocks that %s rely on; keep it, or pass --force (its pieces stay stored either way)",
			name, strings.Join(deps, ", "))
	}
	out := c.OutDir(c.Repos[i])
	c.Repos = append(c.Repos[:i], c.Repos[i+1:]...)
	if err := c.Save(); err != nil {
		return err
	}
	if err := c.Unpublish(name); err != nil {
		return err
	}
	fmt.Fprintf(o.stdout, "removed  %s from the list; its archive stays in %s and its pieces stay stored\n", name, out)
	return nil
}
