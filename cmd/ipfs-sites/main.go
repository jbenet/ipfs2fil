// Command ipfs-sites keeps a list of websites that live on IPFS: it looks
// each one up (DNSLink, ENS, IPNS), records the root it serves now in a lock
// file next to the list, fetches whole sites from trustless gateways
// (every block's hash checked), and writes them out as one CAR.
//
// The list is TOML, named <name>.ipfs-sites.toml (or ipfs-sites.toml):
//
//	[[site]]
//	source = "benet.ai"
//
//	[[site]]
//	source = "/ipns/k51qzi5uqu5d..."
//	name = "blog"
//
// ipfs2fil archives the same lists to Filecoin (--repo mine.ipfs-sites.toml).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/dagfetch"
	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

var (
	version = "dev"
	commit  = ""
)

func versionString() string {
	if version != "dev" {
		if commit != "" {
			return version + " (" + commit + ")"
		}
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const usage = `ipfs-sites keeps a list of websites on IPFS: what each serves, fetched whole.

Usage:
  ipfs-sites [list]                     same as fetch
  ipfs-sites resolve [list]             look up every site's root now; update the lock file
  ipfs-sites fetch [list]               resolve, then download every site, hash-checked
  ipfs-sites car [-o out.car] [list]    fetch, then write every site into one CAR
  ipfs-sites check [list]               say which sites changed since the lock file (exit 1 if any)
  ipfs-sites show [list]                print the lock file's sites
  ipfs-sites add <source> [name] [-note text] [list]
                                        add a site to the list (a new list if there is none)
  ipfs-sites convert <old.sites>        write an old line-format list as <name>.ipfs-sites.toml
  ipfs-sites version

A list is <name>.ipfs-sites.toml, or ipfs-sites.toml; with no list named,
the one in the current folder is used. Its results go in
<name>.ipfs-sites.lock.toml next to it.

A source is a domain with DNSLink (benet.ai), an https URL (its host's DNSLink,
else the X-Ipfs-Path its server sends), an ENS name (vitalik.eth), an IPNS
name (/ipns/k51...; its signed record is checked) or a fixed /ipfs/<cid>.

Run 'ipfs-sites <command> -h' for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	code, err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "ipfs-sites:", err)
		code = 1
	} else if errors.Is(err, flag.ErrHelp) {
		code = 2
	}
	os.Exit(code)
}

// run runs one command and returns its exit code: 1 for check when sites
// changed, else 0 unless err.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := "fetch"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") && !sitelist.IsListName(args[0]) {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "resolve", "fetch", "car", "check":
		return update(ctx, cmd, args, stdout, stderr)
	case "show":
		return 0, show(args, stdout, stderr)
	case "add":
		return 0, add(args, stdout, stderr)
	case "convert":
		return 0, convert(args, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "ipfs-sites", versionString())
		return 0, nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0, nil
	}
	fmt.Fprint(stderr, usage)
	return 2, fmt.Errorf("unknown command %q", cmd)
}

// interleaved lets flags come after positional args.
func interleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// findList is the list named, else the one in the current folder.
func findList(pos []string) (string, error) {
	if len(pos) > 1 {
		return "", fmt.Errorf("name one list, not %d", len(pos))
	}
	if len(pos) == 1 {
		return pos[0], nil
	}
	ents, err := os.ReadDir(".")
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range ents {
		if !e.IsDir() && sitelist.IsListName(e.Name()) && !strings.HasSuffix(strings.ToLower(e.Name()), sitelist.LegacyExt) {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return "", errors.New("no *" + sitelist.Ext + " or " + sitelist.Bare + " here; name a list, or make one with ipfs-sites add <site>")
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d lists here (%s); name one", len(found), strings.Join(found, ", "))
}

// lockPathOf is where list's results go; a line-format list's go beside it
// as <name>.ipfs-sites.lock.toml.
func lockPathOf(list string) string {
	if p := sitelist.LockPath(list); p != "" {
		return p
	}
	return filepath.Join(filepath.Dir(list), sitelist.ListName(list)+sitelist.LockExt)
}

func defaultCache() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "ipfs-sites")
	}
	return filepath.Join(os.TempDir(), "ipfs-sites")
}

func update(ctx context.Context, cmd string, args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("ipfs-sites "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cache := fs.String("cache", defaultCache(), "keep fetched sites here, one CAR per root")
	gws := fs.String("gateways", "", "trustless gateways to fetch from and ask for IPNS records, comma-separated (default: the list's gateways, else $IPFS_SITES_GATEWAYS, else public ones)")
	ens := fs.String("ens", "", "ENS gateway URL with %s for the name (default https://%s.limo)")
	jsonOut := fs.Bool("json", false, "print the results as JSON")
	quiet := fs.Bool("q", false, "print only problems")
	var out *string
	if cmd == "car" {
		out = fs.String("o", "", "write the CAR here (default <list name>.car next to the list)")
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: ipfs-sites %s [flags] [list]\n", cmd)
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return 2, err
	}
	path, err := findList(pos)
	if err != nil {
		return 1, err
	}
	l, err := sitelist.Load(path)
	if err != nil {
		return 1, err
	}
	lockPath := lockPathOf(path)
	lock, err := sitelist.ReadLock(lockPath)
	if err != nil {
		return 1, err
	}
	gateways := dagfetch.GatewaysFromEnv("IPFS_SITES_GATEWAYS")
	if len(l.Gateways) > 0 {
		gateways = l.Gateways
	}
	if *gws != "" {
		gateways = nil
		for _, g := range strings.Split(*gws, ",") {
			if g = strings.TrimSuffix(strings.TrimSpace(g), "/"); g != "" {
				gateways = append(gateways, g)
			}
		}
	}
	f := &dagfetch.Fetcher{Gateways: gateways, Dir: filepath.Join(*cache, "dags")}
	opts := sitelist.Options{Resolver: &sitelist.Resolver{Gateways: gateways, ENSGateway: *ens}}
	if !*quiet && !*jsonOut {
		opts.Log = stderr
	}
	if cmd == "fetch" || cmd == "car" {
		opts.Fetch, opts.Have = f.Fetch, f.Have
		if !*quiet && !*jsonOut {
			fmt.Fprintf(stderr, "Fetching %d sites from %s\n", len(l.Sites), path)
		}
	} else if !*quiet && !*jsonOut {
		fmt.Fprintf(stderr, "Looking up %d sites from %s\n", len(l.Sites), path)
	}
	next := sitelist.Update(ctx, l, lock, opts)
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}

	if cmd == "check" {
		changed := changes(lock, next)
		if *jsonOut {
			return boolCode(len(changed) > 0), json.NewEncoder(stdout).Encode(map[string]any{"changed": changed, "sites": next.Site})
		}
		for _, c := range changed {
			fmt.Fprintln(stdout, c)
		}
		if len(changed) == 0 && !*quiet {
			fmt.Fprintln(stdout, "No site changed since", filepath.Base(lockPath))
		}
		return boolCode(len(changed) > 0), nil
	}

	if err := next.Write(lockPath); err != nil {
		return 1, err
	}
	failed := 0
	for _, e := range next.Site {
		if e.Root == "" || e.Stale {
			failed++
		}
	}
	if cmd == "car" {
		dest := *out
		if dest == "" {
			dest = filepath.Join(filepath.Dir(path), sitelist.ListName(path)+".car")
		}
		n, err := writeCAR(dest, next, f)
		if err != nil {
			return 1, err
		}
		if !*jsonOut {
			fmt.Fprintf(stderr, "Wrote %s: %d sites, %d blocks\n", dest, len(rootsOf(next)), n)
		}
	}
	if *jsonOut {
		return 0, json.NewEncoder(stdout).Encode(next)
	}
	if !*quiet {
		fmt.Fprintf(stderr, "Wrote %s\n", lockPath)
		printSites(stdout, next)
	}
	if failed > 0 {
		return 1, fmt.Errorf("%d of %d sites were not found or fetched this time", failed, len(next.Site))
	}
	return 0, nil
}

func boolCode(b bool) int {
	if b {
		return 1
	}
	return 0
}

// changes says, one line per site, what differs between the lock and now.
func changes(was, now *sitelist.Lock) []string {
	before := map[string]sitelist.Entry{}
	for _, e := range was.Site {
		before[e.Source] = e
	}
	var out []string
	for _, e := range now.Site {
		b, ok := before[e.Source]
		switch {
		case e.Stale || e.Root == "":
			out = append(out, fmt.Sprintf("%s: cannot be looked up: %s", e.Name, e.Error))
		case !ok || b.Root == "":
			out = append(out, fmt.Sprintf("%s: new, %s", e.Name, e.Root))
		case b.Root != e.Root:
			out = append(out, fmt.Sprintf("%s: %s -> %s", e.Name, b.Root, e.Root))
		}
	}
	return out
}

func printSites(w io.Writer, l *sitelist.Lock) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROOT\tSINCE\tHOW")
	for _, e := range l.Site {
		root, since := e.Root, ""
		if root == "" {
			root = "-"
		}
		if e.Since != nil {
			since = e.Since.Format("2006-01-02")
		}
		how := e.How
		if e.Stale {
			how = "stale: " + e.Error
		} else if e.Root == "" {
			how = "not found: " + e.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, root, since, how)
	}
	tw.Flush()
}

func rootsOf(l *sitelist.Lock) []cid.Cid {
	var out []cid.Cid
	seen := map[string]bool{}
	for _, e := range l.Site {
		c, err := cid.Decode(e.Root)
		if err != nil || seen[c.KeyString()] {
			continue
		}
		seen[c.KeyString()] = true
		out = append(out, c)
	}
	return out
}

func show(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ipfs-sites show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print as JSON")
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	path, err := findList(pos)
	if err != nil {
		return err
	}
	lock, err := sitelist.ReadLock(lockPathOf(path))
	if err != nil {
		return err
	}
	if len(lock.Site) == 0 {
		return fmt.Errorf("no %s yet; run ipfs-sites resolve or fetch", filepath.Base(lockPathOf(path)))
	}
	if *jsonOut {
		return json.NewEncoder(stdout).Encode(lock)
	}
	fmt.Fprintf(stdout, "%s, checked %s\n", lock.List, lock.Checked.Format(time.RFC3339))
	printSites(stdout, lock)
	return nil
}

func add(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ipfs-sites add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	note := fs.String("note", "", "a note about the site")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ipfs-sites add <source> [name] [-note text] [list]")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fs.Usage()
		return flag.ErrHelp
	}
	src, name := pos[0], ""
	var listArg []string
	for _, p := range pos[1:] {
		if sitelist.IsListName(p) {
			listArg = append(listArg, p)
		} else if name == "" {
			name = p
		} else {
			return fmt.Errorf("unexpected %q", p)
		}
	}
	path, err := findList(listArg)
	var l *sitelist.List
	if err == nil {
		if _, serr := os.Stat(path); serr == nil {
			if l, err = sitelist.Load(path); err != nil {
				return err
			}
			if l.Legacy {
				return fmt.Errorf("%s is an old line-format list; ipfs-sites convert it first", path)
			}
		}
	} else if len(listArg) == 0 {
		path, err = sitelist.Bare, nil
	}
	if err != nil {
		return err
	}
	// Check the new site parses, and does not clash, before touching the file.
	entry := "\n[[site]]\nsource = " + sitelist.Quote(src) + "\n"
	if name != "" {
		entry += "name = " + sitelist.Quote(name) + "\n"
	}
	if *note != "" {
		entry += "note = " + sitelist.Quote(*note) + "\n"
	}
	var b []byte
	if l != nil {
		b = append(append([]byte{}, l.Raw...), entry...)
	} else {
		b = []byte("# Websites on IPFS to archive. See ipfs-sites help.\n" + entry)
	}
	nl, err := sitelist.ParseTOML(b)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	s := nl.Sites[len(nl.Sites)-1]
	fmt.Fprintf(stdout, "Added %s (%s) to %s\n", s.Name, s.Kind, path)
	return nil
}

func convert(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ipfs-sites convert", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write here (default <name>.ipfs-sites.toml next to it)")
	force := fs.Bool("f", false, "replace a list already there")
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "Usage: ipfs-sites convert [-o out] <old.sites>")
		return flag.ErrHelp
	}
	l, err := sitelist.Load(pos[0])
	if err != nil {
		return err
	}
	dest := *out
	if dest == "" {
		dest = filepath.Join(filepath.Dir(pos[0]), sitelist.ListName(pos[0])+sitelist.Ext)
	}
	if _, err := os.Stat(dest); err == nil && !*force {
		return fmt.Errorf("%s is there already (-f replaces it)", dest)
	}
	b := sitelist.EncodeTOML("", l.Sites)
	if _, err := sitelist.ParseTOML(b); err != nil {
		return fmt.Errorf("converting: %w", err)
	}
	if err := os.WriteFile(dest, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote %s: %d sites\n", dest, len(l.Sites))
	return nil
}
