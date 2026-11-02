// Command git-ipfs-archive turns a git repo into CAR files of IPFS blocks and
// back: history as git packs, a commit log, and a DAG-CBOR record naming
// refs. Each later run with --since writes a delta CAR with one thin pack of
// what is new. A folder of these CARs restores to the exact repo.
//
// git runs it as `git ipfs-archive` too.
package main

import (
	"bufio"
	"context"
	"encoding/binary"
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

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

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
	o := gitarchive.Options{Git: *gitBin, Tool: "git-ipfs-archive " + versionString()}
	if len(set) > 0 {
		o.Instructions = set
	}
	if *since != "" {
		bs, c, err := load(*since, *root)
		if err != nil {
			return err
		}
		if o.Prev, err = gitarchive.LoadRecord(bs.Get, c); err != nil {
			return err
		}
		o.PrevRoot = c
	}
	o.TempDir = filepath.Dir(*out)
	res, err := writeCAR(ctx, *out, func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
		return gitarchive.Archive(ctx, pos[0], o, put)
	})
	if err != nil {
		return err
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
	return nil
}

// writeCAR streams blocks into out. The root is only known at the end, so
// the header is written with a placeholder of the same length and
// rewritten; every record CID is a 36-byte CIDv1 DAG-CBOR sha2-256.
func writeCAR(ctx context.Context, out string, fn func(gitarchive.BlockSink) (*gitarchive.Result, error)) (*gitarchive.Result, error) {
	placeholder, _ := cid.V1Builder{Codec: cid.DagCBOR, MhType: multihash.SHA2_256}.Sum(nil)
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	defer f.Close()
	w := bufio.NewWriterSize(f, 1<<20)
	hdr, err := carHeader(placeholder)
	if err != nil {
		return nil, err
	}
	section := func(parts ...[]byte) error {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		var buf [binary.MaxVarintLen64]byte
		if _, err := w.Write(buf[:binary.PutUvarint(buf[:], uint64(n))]); err != nil {
			return err
		}
		for _, p := range parts {
			if _, err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := section(hdr); err != nil {
		return nil, err
	}
	res, err := fn(func(c cid.Cid, data []byte) error { return section(c.Bytes(), data) })
	if err != nil {
		return nil, err
	}
	if res.Unchanged {
		return res, nil
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	real, err := carHeader(res.Root)
	if err != nil {
		return nil, err
	}
	if len(real) != len(hdr) {
		return nil, fmt.Errorf("root %s has an unexpected shape", res.Root)
	}
	var buf [binary.MaxVarintLen64]byte
	pre := buf[:binary.PutUvarint(buf[:], uint64(len(real)))]
	if _, err := f.WriteAt(append(pre, real...), 0); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return res, os.Rename(tmp, out)
}

// carHeader is a CARv1 header naming one root.
func carHeader(root cid.Cid) ([]byte, error) {
	enc, _ := cbor.CanonicalEncOptions().EncMode()
	return enc.Marshal(struct {
		Roots   []gitarchive.Link `cbor:"roots"`
		Version int               `cbor:"version"`
	}{[]gitarchive.Link{{Cid: root}}, 1})
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
	res, err := gitarchive.Restore(ctx, bs.Get, c, pos[1], gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict, Remotes: *remotes})
	if err != nil {
		return err
	}
	for _, s := range res.Skipped {
		fmt.Fprintln(stderr, "skipped", s)
	}
	fmt.Fprintf(stdout, "restored %s to %s: %d snapshots, %d packs, %d refs; git fsck passed\n", c, pos[1], res.Snapshots, res.Packs, res.Refs)
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
	tmp, err := os.MkdirTemp("", "git-ipfs-archive-verify-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	res, err := gitarchive.Restore(ctx, bs.Get, c, filepath.Join(tmp, "repo.git"), gitarchive.RestoreOptions{Git: *gitBin, Strict: *strict})
	if err != nil {
		return fmt.Errorf("verify failed: %w", err)
	}
	fmt.Fprintf(stdout, "ok %s: %d snapshots, %d packs, %d refs restore and pass git fsck\n", c, res.Snapshots, res.Packs, res.Refs)
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
