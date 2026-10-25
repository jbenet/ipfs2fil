package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/explorer"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/foc"
)

// The archive on disk, as pack leaves it in --out.

func manifestPath(out string) string { return filepath.Join(out, archive.ManifestFile) }

// loadArchive reads the full manifest pack wrote and finds each piece's CAR.
func loadArchive(out string) (*manifest.Manifest, []archive.Piece, error) {
	m, err := manifest.Read(manifestPath(out))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("no archive in %s; run 'ipfs2fil pack' first, or pass --out with the directory pack wrote", out)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w (run 'ipfs2fil pack' again to rewrite it)", manifestPath(out), err)
	}
	var ps []archive.Piece
	for _, p := range m.Pieces {
		path := filepath.Join(out, "pieces", p.PieceCID+".car")
		if _, err := os.Stat(path); err != nil {
			return nil, nil, fmt.Errorf("piece %d (%s) is missing from %s; run 'ipfs2fil pack' again to rewrite it", p.Index, p.PieceCID, filepath.Join(out, "pieces"))
		}
		ps = append(ps, archive.Piece{Piece: p, Path: path})
	}
	return m, ps, nil
}

// saveManifest writes the full manifest atomically, and refreshes the local
// explorer so it shows the new copies.
func saveManifest(out string) func(*manifest.Manifest) error {
	return func(m *manifest.Manifest) error {
		tmp := manifestPath(out) + ".tmp"
		if err := m.Write(tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, manifestPath(out)); err != nil {
			return err
		}
		if dir := filepath.Join(out, "explorer"); isDir(dir) {
			return explorer.WriteManifest(dir, m)
		}
		return nil
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Storage. Commands that talk to Filecoin share these flags.

type storeOpts struct {
	keyFile  string
	rpc      string
	network  string
	address  string
	readOnly bool // status and verify need no key, only the paying address
}

var storeFlags storeOpts

func addStoreFlags(fs *flag.FlagSet) {
	storeFlags.readOnly = false
	fs.StringVar(&storeFlags.keyFile, "key-file", "", "file holding the wallet's private key (default $"+foc.EnvKeyFile+", else $"+foc.EnvKey+")")
	fs.StringVar(&storeFlags.network, "network", "mainnet", "mainnet or calibration")
	fs.StringVar(&storeFlags.rpc, "rpc", "", "Filecoin RPC URL (default: the public glif endpoint for --network)")
}

// addReadStoreFlags is addStoreFlags for commands that only read from chain.
func addReadStoreFlags(fs *flag.FlagSet) {
	addStoreFlags(fs)
	storeFlags.readOnly = true
	fs.StringVar(&storeFlags.address, "address", "", "with no wallet key, the 0x address that paid for the archive (default $"+foc.EnvAddress+")")
}

// funder is a store that can say what funding a push needs (FOC can).
type funder interface {
	Plan(ctx context.Context, archive string, copies int, sizes []int64) (*foc.Funding, error)
}

// checkKey fails early, before a long pack, when no wallet key is set. Tests
// replace it along with openStore.
var checkKey = func() error {
	_, err := foc.LoadKey(storeFlags.keyFile)
	return err
}

// openStore connects to the storage backend. Tests replace it.
var openStore = func(ctx context.Context, o *opts) (store.Store, io.Closer, error) {
	rpc := storeFlags.rpc
	if rpc == "" {
		switch storeFlags.network {
		case "mainnet":
			rpc = foc.MainnetRPC
		case "calibration", "calibnet":
			rpc = foc.CalibrationRPC
		default:
			return nil, nil, fmt.Errorf("unknown --network %q; use mainnet or calibration", storeFlags.network)
		}
	}
	var cfg foc.Config
	if storeFlags.readOnly {
		c, err := foc.ReadConfig(rpc, storeFlags.keyFile, storeFlags.address)
		if err != nil {
			return nil, nil, err
		}
		cfg = c
	} else {
		key, err := foc.LoadKey(storeFlags.keyFile)
		if err != nil {
			return nil, nil, err
		}
		cfg = foc.Config{RPC: rpc, Key: key}
	}
	s, c, err := foc.Open(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return s, c, nil
}
