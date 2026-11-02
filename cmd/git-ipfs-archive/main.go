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
	"sort"
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
  git-ipfs-archive [flags] <folder> -o <out.car> archive every repo under a folder as one set
  git-ipfs-archive [flags] <https URL> -o <out.car>
                                                 clone a repo and archive it
  git-ipfs-archive [flags] <repo or URL>... -o <out.car>
                                                 archive several repos as one set
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
	trustConfig := fs.Bool("trust-config", false, "let a cloned repo's "+gitarchive.ConfigFile+" turn on costly settings (tip folders of every branch, submodules, LFS fetch), as a local repo's may")
	var lfsFetch optBool
	fs.Var(&lfsFetch, "lfs-fetch", "run git lfs fetch --all first, to store LFS objects only the server has (default: the repo's "+gitarchive.ConfigFile+", else store the local ones)")
	var subs optBool
	fs.Var(&subs, "submodules", "also archive every submodule's repo, recursively, from the repo's modules/ or a clone of its https URL (default: the repo's "+gitarchive.ConfigFile+", else only list them)")
	set := kvFlag{}
	fs.Var(set, "set", "key=value instruction recorded for whatever stores the archive (repeatable), e.g. ipfs2fil.copies=3")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive [flags] <repo, folder or https URL>... -o <out.car>")
		fs.PrintDefaults()
	}
	pos, err := interleaved(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || *out == "" {
		fs.Usage()
		return flag.ErrHelp
	}
	o := gitarchive.Options{Git: *gitBin, Tool: "git-ipfs-archive " + versionString(), Tips: *tips, NoConfig: *noConfig, Submodules: subs.v, LFSFetch: lfsFetch.v}
	if len(set) > 0 {
		o.Instructions = set
	}
	o.TempDir = filepath.Dir(*out)
	in, cleanup, err := inputs(ctx, pos, *gitBin, o.TempDir, !*trustConfig, stderr)
	defer cleanup()
	if err != nil {
		return err
	}
	var prevRoot cid.Cid
	var bs *gitarchive.Blocks
	if *since != "" {
		if bs, prevRoot, err = load(*since, *root); err != nil {
			return err
		}
		defer bs.Close()
		o.Have, o.Get = bs.Has, bs.Get
	}
	if in.set {
		return archiveSet(ctx, *out, in.repos, o, bs, prevRoot, stdout, stderr)
	}
	if bs != nil {
		if o.Prev, err = gitarchive.LoadRecord(bs.Get, prevRoot); err != nil {
			if gitarchive.IsSet(bs.Get, prevRoot) {
				return fmt.Errorf("--since names a set of repos; archive the same folder or list of repos")
			}
			return err
		}
		o.PrevRoot = prevRoot
	}
	r := in.repos[0]
	o.UntrustedConfig = r.Untrusted
	res, err := gitarchive.WriteCAR(*out, func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
		return gitarchive.Archive(ctx, r.Path, o, put)
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

type inputSet struct {
	repos []gitarchive.Input
	set   bool // archive as a set: a folder, or more than one input
}

// inputs turns the command line into repos: a URL is cloned (its config
// untrusted unless trust), a folder that is not a repo stands for every
// repo under it. One repo or URL is archived alone; anything else is a set.
// cleanup removes the clones.
func inputs(ctx context.Context, pos []string, gitBin, tmp string, untrusted bool, stderr io.Writer) (inputSet, func(), error) {
	var in inputSet
	var clones []string
	cleanup := func() {
		for _, c := range clones {
			os.RemoveAll(c)
		}
	}
	in.set = len(pos) > 1
	for _, p := range pos {
		if gitarchive.IsCloneURL(p) {
			name := gitarchive.URLName(p)
			fmt.Fprintf(stderr, "cloning %s\n", gitarchive.StripCredentials(strings.TrimPrefix(p, "git+")))
			dir, err := gitarchive.Clone(ctx, gitBin, p, tmp)
			if err != nil {
				return in, cleanup, err
			}
			clones = append(clones, dir)
			in.repos = append(in.repos, gitarchive.Input{Name: name, Path: dir, Untrusted: untrusted})
			continue
		}
		if strings.Contains(p, "://") {
			return in, cleanup, fmt.Errorf("%s: only https URLs are cloned", p)
		}
		if gitarchive.IsGitRepo(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return in, cleanup, err
			}
			name := filepath.Base(abs)
			if name == ".git" {
				name = filepath.Base(filepath.Dir(abs))
			}
			in.repos = append(in.repos, gitarchive.Input{Name: name, Path: p})
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			return in, cleanup, err
		}
		if !st.IsDir() {
			return in, cleanup, fmt.Errorf("%s is not a git repo or a folder", p)
		}
		found, notes, err := gitarchive.FindRepos(p)
		if err != nil {
			return in, cleanup, err
		}
		for _, n := range notes {
			fmt.Fprintf(stderr, "note: %s\n", n)
		}
		if len(found) == 0 {
			return in, cleanup, fmt.Errorf("no git repos under %s", p)
		}
		if len(pos) > 1 {
			abs, _ := filepath.Abs(p)
			for i := range found {
				found[i].Name = filepath.Base(abs) + "/" + found[i].Name
			}
		}
		in.set = true
		in.repos = append(in.repos, found...)
	}
	return in, cleanup, nil
}

func archiveSet(ctx context.Context, out string, repos []gitarchive.Input, o gitarchive.Options, bs *gitarchive.Blocks, prevRoot cid.Cid, stdout, stderr io.Writer) error {
	so := gitarchive.SetOptions{Options: o, Log: func(f string, a ...any) { fmt.Fprintf(stderr, "reading "+f, a...) }}
	if bs != nil {
		prev, err := gitarchive.LoadSet(bs.Get, prevRoot)
		if err != nil {
			if gitarchive.IsRecord(bs.Get, prevRoot) {
				return fmt.Errorf("--since names one repo's archive, not a set; archive that repo alone")
			}
			return err
		}
		so.Prev, so.PrevRoot = prev, prevRoot
	}
	res, err := gitarchive.ArchiveSetCAR(ctx, out, repos, so)
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
	fmt.Fprintf(stdout, "%s\n", res.Root)
	var objects, commits int
	var bytes int64
	for _, m := range res.Repos {
		if m.Result == nil {
			continue
		}
		line := "unchanged"
		if !m.Unchanged {
			objects += m.Objects
			bytes += m.PackBytes
			commits += m.Record.Commits
			line = fmt.Sprintf("%d new objects, %s", m.Objects, size(m.PackBytes))
		}
		if len(m.Bases) > 0 {
			line += ", packed against " + strings.Join(m.Bases, ", ")
		}
		fmt.Fprintf(stderr, "  %s: %s\n", m.Name, line)
	}
	fmt.Fprintf(stderr, "wrote %s: a set of %d repos, %d new objects in %s of packs, %d commits\n",
		out, len(res.Record.Repos), objects, size(bytes), commits)
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
	var only listFlag
	fs.Var(&only, "only", "for a set: restore only the repo of this name (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: git-ipfs-archive restore [flags] <car or dir> <dest>")
		fmt.Fprintln(stderr, "A set restores each repo to <dest>/<name>.git.")
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
	ro := gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict, Remotes: *remotes, Recurse: *recurse}
	if gitarchive.IsSet(bs.Get, c) {
		rs, err := gitarchive.RestoreSet(ctx, bs.Get, c, pos[1], only, ro)
		if err != nil {
			return err
		}
		for _, name := range sortedKeys(rs) {
			r := rs[name]
			for _, s := range r.Skipped {
				fmt.Fprintf(stderr, "%s: skipped %s\n", name, s)
			}
			fmt.Fprintf(stdout, "restored %s to %s: %d snapshots, %d packs, %d refs; git fsck passed\n", name, gitarchive.RestoreDir(pos[1], name), r.Snapshots, r.Packs, r.Refs)
		}
		return nil
	}
	if len(only) > 0 {
		return fmt.Errorf("--only is for sets; %s is one repo's archive", c)
	}
	res, err := gitarchive.Restore(ctx, bs.Get, c, pos[1], ro)
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
	if gitarchive.IsSet(bs.Get, c) {
		rs, err := gitarchive.RestoreSet(ctx, bs.Get, c, filepath.Join(tmp, "set"), nil, gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict, Recurse: true})
		if err != nil {
			return fmt.Errorf("verify failed: %w", err)
		}
		for _, name := range sortedKeys(rs) {
			fmt.Fprintf(stdout, "ok %s: %d snapshots, %d packs, %d refs\n", name, rs[name].Snapshots, rs[name].Packs, rs[name].Refs)
		}
		fmt.Fprintf(stdout, "ok %s: all %d repos restore and pass git fsck\n", c, len(rs))
		return nil
	}
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
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if set, err := gitarchive.LoadSet(bs.Get, c); err == nil {
		type member struct {
			Name      string `json:"name"`
			Note      string `json:"note,omitempty"`
			Snapshots []snap `json:"snapshots"`
		}
		out := struct {
			Root  string   `json:"root"`
			Kind  string   `json:"kind"`
			Prev  string   `json:"prev,omitempty"`
			Tool  string   `json:"tool,omitempty"`
			Repos []member `json:"repos"`
		}{Root: c.String(), Kind: set.Kind, Tool: set.Tool, Repos: []member{}}
		if set.Prev != nil {
			out.Prev = set.Prev.String()
		}
		for _, m := range set.Repos {
			ss, err := snaps(bs, m.Record.Cid)
			if err != nil {
				return fmt.Errorf("%s: %w", m.Name, err)
			}
			out.Repos = append(out.Repos, member{m.Name, m.Note, ss})
		}
		return enc.Encode(out)
	}
	out, err := snaps(bs, c)
	if err != nil {
		return err
	}
	return enc.Encode(out)
}

type snap struct {
	Root   string             `json:"root"`
	Record *gitarchive.Record `json:"record"`
}

func snaps(bs *gitarchive.Blocks, c cid.Cid) ([]snap, error) {
	chain, err := gitarchive.Chain(bs.Get, c)
	if err != nil {
		return nil, err
	}
	var out []snap
	for _, s := range chain {
		out = append(out, snap{s.Root.String(), s.Record})
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// listFlag collects a repeatable flag; commas separate values too.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(s string) error {
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*l = append(*l, v)
		}
	}
	return nil
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
