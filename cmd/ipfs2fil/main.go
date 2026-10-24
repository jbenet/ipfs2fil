// Command ipfs2fil archives an IPFS repo to Filecoin.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jbenet/ipfs2fil/internal/repo"
)

var version = "dev" // set by the release build

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
		fmt.Println("ipfs2fil", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	case "scan", "pack", "push", "status", "explore", "verify":
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
	return errors.New(cmd + " is not implemented yet")
}
