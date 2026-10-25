// Command ipfs2fil archives an IPFS repo to Filecoin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"

	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
	"github.com/multiformats/go-multihash"
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

const usage = `ipfs2fil archives an IPFS repo to Filecoin.

Usage:
  ipfs2fil <command> [--repo <path>] [flags]

Commands:
  scan      Read the repo and say what an archive would hold and cost
  pack      Build the archive and its pieces on disk
  push      Upload pieces that are not stored yet (3 copies by default)
  status    Show copies, funding and proofs for an archive
  explore   Write or serve the HTML explorer
  verify    Fetch pieces back and check every block
  version   Print the version

The repo is --repo, else $IPFS_PATH, else ~/.ipfs. It is only ever read.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if err := run(context.Background(), cmd, args); err != nil {
		fmt.Fprintln(os.Stderr, "ipfs2fil:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	repoPath := fs.String("repo", "", "path to the IPFS repo")
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("ipfs2fil", versionString())
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	case "explore":
		return explore(ctx, args)
	case "scan", "pack", "push", "status", "verify":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := repo.Discover(*repoPath)
	if err != nil {
		return err
	}
	v, err := repo.DetectVersion(path)
	if err != nil {
		return err
	}
	fmt.Printf("repo %s (fs-repo %d)\n", path, v)
	if cmd == "scan" {
		return scan(ctx, path)
	}
	return errors.New(cmd + " is not implemented yet")
}

// explore refreshes or serves the explorer of an archive on disk.
func explore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	dir := fs.String("archive", ".", "archive directory, the one holding index/ and explorer/")
	serve := fs.Bool("serve", false, "serve the archive and its explorer on --addr")
	addr := fs.String("addr", "127.0.0.1:8484", "address for --serve")
	demo := fs.Bool("demo", false, "write a made-up archive explorer into --archive (a new temp dir if unset) and serve it")
	scale := fs.Int("scale", 1, "with --demo, how big the made-up archive is")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *demo {
		if !isSet(fs, "archive") {
			d, err := os.MkdirTemp("", "ipfs2fil-demo-")
			if err != nil {
				return err
			}
			*dir = d
		}
		m, entries := explorer.Demo(*scale)
		if err := os.MkdirAll(filepath.Join(*dir, "index"), 0o755); err != nil {
			return err
		}
		if err := m.Write(filepath.Join(*dir, "index", "manifest.json")); err != nil {
			return err
		}
		if err := explorer.Write(filepath.Join(*dir, "explorer"), m, entries); err != nil {
			return err
		}
		fmt.Printf("wrote a demo archive explorer to %s (%d entries)\n", *dir, len(entries))
		*serve = true
	} else {
		m, err := manifest.Read(filepath.Join(*dir, "index", "manifest.json"))
		if err != nil {
			return fmt.Errorf("%w; pass --archive with the directory pack wrote", err)
		}
		if err := explorer.WriteManifest(filepath.Join(*dir, "explorer"), m); err != nil {
			return err
		}
		fmt.Printf("explorer in %s is up to date with index/manifest.json\n", filepath.Join(*dir, "explorer"))
	}
	if !*serve {
		return nil
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	return explorer.Serve(ctx, *dir, *addr, func(url string) {
		fmt.Printf("serving %s at %s (ctrl-c stops)\n", *dir, url)
	})
}

func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// scan prints what the repo holds. Archive contents and costs come later.
func scan(ctx context.Context, path string) error {
	r, err := repo.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	var n, size int
	err = r.ForEachBlock(ctx, func(_ multihash.Multihash, s int) error {
		n++
		size += s
		return nil
	})
	if err != nil {
		return err
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		return err
	}
	var rec int
	for _, p := range pins {
		if p.Mode == repo.Recursive {
			rec++
		}
	}
	fmt.Printf("%d blocks, %d bytes\n%d pins (%d recursive, %d direct)\n", n, size, len(pins), rec, len(pins)-rec)
	if c, ok, err := r.MFSRoot(ctx); err != nil {
		return err
	} else if ok {
		fmt.Printf("MFS root %s\n", c)
	}
	return nil
}
