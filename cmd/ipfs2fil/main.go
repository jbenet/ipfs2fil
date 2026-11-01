// Command ipfs2fil archives an IPFS repo to Filecoin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
)

// Set by the release build (.goreleaser.yaml).
var (
	version = "dev"
	commit  = ""
)

// versionString is the release version, or for `go install` builds the
// module version and VCS revision the toolchain recorded.
func versionString() string {
	v, c := version, commit
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && c == "" && len(s.Value) >= 7 {
				c = s.Value[:7]
			}
		}
	}
	if c != "" {
		return v + " (" + c + ")"
	}
	return v
}

// command is one subcommand. help is the man-style text `ipfs2fil help <name>`
// prints above the flags.
type command struct {
	name    string
	summary string
	help    string
	flags   func(fs *flag.FlagSet, o *opts)
	run     func(ctx context.Context, o *opts, args []string) error
}

var commands = map[string]*command{}

func register(c *command) { commands[c.name] = c }

// order is how the usage lists commands.
var order = []string{"wallet", "scan", "pack", "push", "up", "status", "explore", "verify", "export", "version"}

func usage(w io.Writer) {
	fmt.Fprint(w, `ipfs2fil archives an IPFS repo to Filecoin.

Usage:
  ipfs2fil <command> [flags]

Commands:
`)
	names := append([]string(nil), order...)
	for n := range commands {
		if !contains(names, n) {
			names = append(names, n)
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return index(order, names[i]) < index(order, names[j]) })
	for _, n := range names {
		if c := commands[n]; c != nil {
			fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
		}
	}
	fmt.Fprint(w, `
The repo is --repo, else $IPFS_PATH, else ~/.ipfs. It is only ever read.
Run 'ipfs2fil help <command>' for details.
`)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	interrupted := ctx.Err() != nil
	stop()
	switch {
	case err == nil:
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case interrupted:
		fmt.Fprintln(os.Stderr, "\nipfs2fil: stopped. Run the same command again to pick up where it left off.")
		os.Exit(130)
	default:
		fmt.Fprintln(os.Stderr, "ipfs2fil:", err)
		os.Exit(1)
	}
}

// errUsage means the usage was already printed.
var errUsage = errors.New("usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}
	name, args := args[0], args[1:]
	switch name {
	case "--version", "-v":
		name = "version"
	case "help", "--help", "-h":
		if len(args) == 0 {
			usage(stdout)
			return nil
		}
		c := commands[args[0]]
		if c == nil {
			return fmt.Errorf("no command %q; run 'ipfs2fil help' for the list", args[0])
		}
		o := &opts{stdout: stdout, stderr: stderr}
		printHelp(stdout, c, newFlagSet(c, o))
		return nil
	}
	c := commands[name]
	if c == nil {
		fmt.Fprintf(stderr, "ipfs2fil: no command %q\n\n", name)
		usage(stderr)
		return errUsage
	}
	o := &opts{stdout: stdout, stderr: stderr}
	fs := newFlagSet(c, o)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(stdout, c, fs)
			return err
		}
		fmt.Fprintf(stderr, "ipfs2fil %s: %v\nRun 'ipfs2fil help %s' for its flags.\n", c.name, err, c.name)
		return errUsage
	}
	o.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return c.run(ctx, o, fs.Args())
}

func newFlagSet(c *command, o *opts) *flag.FlagSet {
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	if c.flags != nil {
		c.flags(fs, o)
	}
	return fs
}

func printHelp(w io.Writer, c *command, fs *flag.FlagSet) {
	fmt.Fprintf(w, "ipfs2fil %s: %s\n\n%s\n", c.name, c.summary, strings.TrimSpace(c.help))
	n := 0
	fs.VisitAll(func(*flag.Flag) { n++ })
	if n == 0 {
		return
	}
	fmt.Fprint(w, "\nFlags:\n")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func contains(s []string, v string) bool { return index(s, v) < len(s) }

func index(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return len(s)
}

func init() {
	register(&command{
		name:    "version",
		summary: "Print the version",
		help:    "Prints the release version and commit.",
		run: func(_ context.Context, o *opts, _ []string) error {
			fmt.Fprintln(o.stdout, "ipfs2fil", versionString())
			return nil
		},
	})
}
