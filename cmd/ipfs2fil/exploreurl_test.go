package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

func TestExploreURL(t *testing.T) {
	dir := t.TempDir()
	m := &manifest.Manifest{Root: "bafyroot", Copies: []manifest.Copy{{Provider: "1", DataSetID: "7"}}}
	if err := m.Write(manifestPath(dir)); err != nil {
		t.Fatal(err)
	}
	out, errs, err := ipfs2fil(t, "explore-url", "--out", dir)
	if err != nil || out != "https://dweb.link/ipfs/bafyroot/explorer/\n" || errs != "" {
		t.Fatalf("got %q %q %v", out, errs, err)
	}
	out, _, _ = ipfs2fil(t, "explore-url", "--out", dir, "--gateway", "https://ipfs.io/")
	if out != "https://ipfs.io/ipfs/bafyroot/explorer/\n" {
		t.Fatalf("got %q", out)
	}
	// not stored yet: the URL, and a note on stderr
	m.Copies = nil
	if err := m.Write(manifestPath(dir)); err != nil {
		t.Fatal(err)
	}
	_, errs, _ = ipfs2fil(t, "explore-url", "--out", dir)
	if !strings.Contains(errs, "not stored yet") {
		t.Fatalf("stderr %q", errs)
	}
	if _, _, err := ipfs2fil(t, "explore-url", "--out", filepath.Join(dir, "none")); err == nil {
		t.Fatal("no error for a missing archive")
	}
}
