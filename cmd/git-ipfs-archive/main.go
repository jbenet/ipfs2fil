// Command git-ipfs-archive turns a git repo into CAR files of IPFS blocks and
// back: history as git packs, a commit log, and a DAG-CBOR record naming
// refs. Each later run with --since writes a delta CAR with one thin pack of
// what is new. A folder of these CARs restores to the exact repo.
//
// git runs it as `git ipfs-archive` too.
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
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/gitarchive"
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

const usage = `git-ipfs-archive turns a git repo into CAR files and back.

Usage:
  git-ipfs-archive [flags] <repo> -o <out.car>   archive a repo (a delta with --since)
  git-ipfs-archive restore [flags] <car or dir> <dest>
  git-ipfs-archive verify [flags] <car or dir>
  git-ipfs-archive show [flags] <car or dir>
  git-ipfs-archive version

Run 'git-ipfs-archive <command> -h' for a command's flags.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "git-ipfs-archive:", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "restore":
		return cmdRestore(ctx, args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(ctx, args[1:], stdout, stderr)
	case "show":
		return cmdShow(args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "git-ipfs-archive", versionString())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return cmdArchive(ctx, args, stdout, stderr)
}

type kvFlag map[string]string

func (k kvFlag) String() string { return "" }
func (k kvFlag) Set(s string) error {
	key, v, ok := strings.Cut(s, "=")
	if !ok || key == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	k[key] = v
	return nil
}

// interleaved lets flags come after positional args, as git users expect.
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

func cmdArchive(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("git-ipfs-archive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write the CAR here (required)")
	since := fs.String("since", "", "build on this earlier archive (a CAR, or a directory of them): write only what is new")
	root := fs.String("root", "", "with --since: the snapshot to build on, when the CARs hold more than one")
	gitBin := fs.String("git", "", "git binary to run")
	tips := fs.String("tips", "", "store a folder of the files at these refs: none, default (HEAD's branch), all (every branch), or ref names, comma-separated (default: the repo's "+gitarchive.ConfigFile+", else none)")
	noConfig := fs.Bool("no-config", false, "ignore the repo's "+gitarchive.ConfigFile)
	var lfsFetch optBool
	fs.Var(&lfsFetch, "lfs-fetch", "run git lfs fetch --all first, to store LFS objects only the server has (default: the repo's "+gitarchive.ConfigFile+", else store the local ones)")
	var subs optBool
	fs.Var(&subs, "submodules", "also archive every submodule's repo, recursively, from the repo's modules/ or a clone of its https URL (default: the repo's "+gitarchive.ConfigFile+", else only list them)")
	set := kvFlag{}
	fs.Var(set, "set", "key=value instruction recorded for whatever stores the archive (repeatable), e.g. ipfs2fil.copies=3")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive [flags] <repo> -o <out.car>")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *out == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	o := gitarchive.Options{Git: *gitBin, Tool: "git-ipfs-archive " + versionString(), Tips: *tips, NoConfig: *noConfig, Submodules: subs.v, LFSFetch: lfsFetch.v}
	if len(set) > 0 {
		o.Instructions = set
	}
	if *since != "" {
		bs, c, err := load(*since, *root)
		if err != nil {
			return err
		}
		defer bs.Close()
		if o.Prev, err = gitarchive.LoadRecord(bs.Get, c); err != nil {
			return err
		}
		o.PrevRoot, o.Have, o.Get = c, bs.Has, bs.Get
	}
	o.TempDir = filepath.Dir(*out)
	res, err := gitarchive.WriteCAR(*out, func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
		return gitarchive.Archive(ctx, pos[0], o, put)
	})
	if err != nil {
		return err
	}
	for _, n := range res.Notes {
		fmt.Fprintf(stderr, "note: %s\n", n)
	}
	if res.Unchanged {
		fmt.Fprintf(stdout, "nothing changed since %s; no CAR written\n", res.Root)
		return nil
	}
	kind := "full"
	if o.Prev != nil {
		kind = "delta"
	}
	fmt.Fprintf(stdout, "%s\n", res.Root)
	fmt.Fprintf(stderr, "wrote %s (%s): %d refs, %d new objects in a %s pack, %d commits\n",
		*out, kind, len(res.Record.Refs), res.Objects, size(res.PackBytes), res.Record.Commits)
	if n := res.Record.LFSObjects; n > 0 {
		fmt.Fprintf(stderr, "  and %d new LFS objects\n", n)
	}
	for _, t := range res.Record.Tips {
		fmt.Fprintf(stderr, "files of %s: %s\n", t.Ref, t.Root)
	}
	return nil
}

func load(path, root string) (*gitarchive.Blocks, cid.Cid, error) {
	bs := gitarchive.NewBlocks()
	if err := bs.Load(path); err != nil {
		return nil, cid.Undef, err
	}
	if root != "" {
		c, err := cid.Decode(root)
		return bs, c, err
	}
	c, err := bs.Latest()
	return bs, c, err
}

func cmdRestore(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "snapshot to restore, when the CARs hold more than one (default: the newest)")
	strict := fs.Bool("strict", false, "run git fsck --strict")
	remotes := fs.Bool("remotes", false, "configure the recorded remotes")
	recurse := fs.Bool("recurse", false, "also restore archived submodules, each in <dest>/modules/<name>")
	gitBin := fs.String("git", "", "git binary to run")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive restore [flags] <car or dir> <dest>")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		fs.Usage()
		return flag.ErrHelp
	}
	bs, c, err := load(pos[0], *root)
	if err != nil {
		return err
	}
	defer bs.Close()
	res, err := gitarchive.Restore(ctx, bs.Get, c, pos[1], gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict, Remotes: *remotes, Recurse: *recurse})
	if err != nil {
		return err
	}
	for _, s := range res.Skipped {
		fmt.Fprintln(stderr, "skipped", s)
	}
	fmt.Fprintf(stdout, "restored %s to %s: %d snapshots, %d packs, %d refs, %d LFS objects; git fsck passed\n", c, pos[1], res.Snapshots, res.Packs, res.Refs, res.LFS)
	for _, r := range res.Restored {
		fmt.Fprintf(stdout, "restored submodule repo %s\n", filepath.Join(pos[1], r))
	}
	return nil
}

func cmdVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "snapshot to verify (default: the newest)")
	strict := fs.Bool("strict", false, "run git fsck --strict")
	gitBin := fs.String("git", "", "git binary to run")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive verify [flags] <car or dir>")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	bs, c, err := load(pos[0], *root)
	if err != nil {
		return err
	}
	defer bs.Close()
	tmp, err := os.MkdirTemp("", "git-ipfs-archive-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	res, err := gitarchive.Restore(ctx, bs.Get, c, filepath.Join(tmp, "repo.git"), gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict, Recurse: true})
	if err != nil {
		return fmt.Errorf("verify failed: %w", err)
	}
	fmt.Fprintf(stdout, "ok %s: %d snapshots, %d packs, %d refs restore and pass git fsck", c, res.Snapshots, res.Packs, res.Refs)
	if n := len(res.Restored); n > 0 {
		fmt.Fprintf(stdout, ", with %d submodule repo", n)
		if n > 1 {
			fmt.Fprint(stdout, "s")
		}
	}
	fmt.Fprintln(stdout)
	return nil
}

func cmdShow(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "snapshot to show (default: the newest)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive show [flags] <car or dir>")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return flag.ErrHelp
	}
	bs, c, err := load(pos[0], *root)
	if err != nil {
		return err
	}
	defer bs.Close()
	chain, err := gitarchive.Chain(bs.Get, c)
	if err != nil {
		return err
	}
	type snap struct {
		Root   string             `json:"root"`
		Record *gitarchive.Record `json:"record"`
	}
	var out []snap
	for _, s := range chain {
		out = append(out, snap{s.Root.String(), s.Record})
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// optBool is a bool flag that knows whether it was given.
type optBool struct{ v *bool }

func (b *optBool) String() string {
	if b == nil || b.v == nil {
		return ""
	}
	return strconv.FormatBool(*b.v)
}

func (b *optBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.v = &v
	return nil
}

func (b *optBool) IsBoolFlag() bool { return true }
