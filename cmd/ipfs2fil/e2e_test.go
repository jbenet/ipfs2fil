package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/memstore"
)

// useMemStore points the store commands at one in-memory store for the test.
func useMemStore(t *testing.T, providers int) *memstore.Store {
	t.Helper()
	ms := memstore.New(providers)
	old, oldKey := openStore, checkKey
	openStore = func(context.Context, *opts) (store.Store, io.Closer, error) { return ms, io.NopCloser(nil), nil }
	checkKey = func() error { return nil }
	t.Cleanup(func() { openStore, checkKey = old, oldKey })
	return ms
}

// fundedStore reports funding the way the Filecoin store does.
type fundedStore struct{ store.Store }

func (f fundedStore) Status(ctx context.Context, key string) (store.Status, error) {
	st, err := f.Store.Status(ctx, key)
	if err != nil {
		return st, err
	}
	st.FundedDays, st.Reserve, st.ReserveDataSets = 40, "3", 6
	return st, nil
}

// TestEndToEnd: fixture repo -> pack -> push to a fake store -> re-run uploads
// nothing -> explorer written; then status and verify read it back.
func TestEndToEnd(t *testing.T) {
	ms := useMemStore(t, 5)
	repoDir := fixture(t, "v18")
	out := filepath.Join(t.TempDir(), "archive")
	before := treeState(t, repoDir)
	t.Cleanup(func() {
		if after := treeState(t, repoDir); after != before {
			t.Errorf("the repo changed:\nbefore %s\nafter  %s", before, after)
		}
	})

	stdout, _, err := ipfs2fil(t, "up", "--repo", repoDir, "--out", out, "--yes")
	if err != nil {
		t.Fatalf("up: %v\n%s", err, stdout)
	}
	m, err := manifest.Read(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Pieces) == 0 || len(m.Copies) != 3 {
		t.Fatalf("after up: %d pieces, %d copies", len(m.Pieces), len(m.Copies))
	}
	if got, want := ms.Uploads(), 3*len(m.Pieces); got != want {
		t.Errorf("uploads = %d, want %d", got, want)
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != len(m.Pieces) {
			t.Errorf("copy %s records %d of %d pieces", c.Provider, len(c.PieceIDs), len(m.Pieces))
		}
	}
	for _, p := range []string{"explorer/index.html", "index/manifest.json"} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("explorer not written: %v", err)
		}
	}

	// Same repo again: same pieces, nothing uploaded.
	stdout, _, err = ipfs2fil(t, "up", "--repo", repoDir, "--out", out, "--yes")
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if !strings.Contains(stdout, "0 changed since the last pack") || !strings.Contains(stdout, "nothing to upload") {
		t.Errorf("second up output:\n%s", stdout)
	}
	if got, want := ms.Uploads(), 3*len(m.Pieces); got != want {
		t.Errorf("re-run uploaded %d more pieces", got-want)
	}

	// push with the manifest forgetting the copies still uploads nothing:
	// the store says it has every piece.
	m.Copies = nil
	if err := m.Write(filepath.Join(out, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = ipfs2fil(t, "push", "--out", out, "--yes")
	if err != nil || !strings.Contains(stdout, "uploaded 0 pieces") {
		t.Errorf("push after losing copies: %v\n%s", err, stdout)
	}
	if got, want := ms.Uploads(), 3*len(m.Pieces); got != want {
		t.Errorf("push re-uploaded %d pieces", got-want)
	}

	stdout, _, err = ipfs2fil(t, "status", "--out", out)
	if err != nil || !strings.Contains(stdout, "copies   3") {
		t.Errorf("status: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "status", "--out", out, "--json")
	if err != nil || !strings.Contains(stdout, `"dataSets": [`) || !strings.Contains(stdout, `"provider": `) || strings.Contains(stdout, `"Provider"`) {
		t.Errorf("status --json: %v\n%s", err, stdout)
	}
	// A store that knows its funding shows the account's reserve too.
	openStore = func(context.Context, *opts) (store.Store, io.Closer, error) {
		return fundedStore{ms}, io.NopCloser(nil), nil
	}
	stdout, _, err = ipfs2fil(t, "status", "--out", out)
	if err != nil || !strings.Contains(stdout, "funded   40 days (plus 3 USDFC reserved by 6 data sets)") {
		t.Errorf("status with a reserve: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "status", "--out", out, "--json")
	if err != nil || !strings.Contains(stdout, `"reserve": "3"`) || !strings.Contains(stdout, `"reserveDataSets": 6`) {
		t.Errorf("status --json with a reserve: %v\n%s", err, stdout)
	}
	openStore = func(context.Context, *opts) (store.Store, io.Closer, error) { return ms, io.NopCloser(nil), nil }

	stdout, _, err = ipfs2fil(t, "verify", "--out", out)
	if err != nil || !strings.Contains(stdout, " 0 bad") {
		t.Errorf("verify: %v\n%s", err, stdout)
	}

	// A copy with a flipped byte fails verify, and says what to do.
	ms.Corrupt(m.Pieces[0].PieceCID, 100)
	_, _, err = ipfs2fil(t, "verify", "--out", out)
	if err == nil || !strings.Contains(err.Error(), "ipfs2fil push") {
		t.Errorf("verify of a corrupt piece: %v", err)
	}
}

func TestPushAsksFirst(t *testing.T) {
	ms := useMemStore(t, 3)
	out := filepath.Join(t.TempDir(), "archive")
	if _, _, err := ipfs2fil(t, "pack", "--repo", fixture(t, "v11"), "--out", out); err != nil {
		t.Fatal(err)
	}
	old := stdin
	t.Cleanup(func() { stdin = old })

	stdin = strings.NewReader("n\n")
	_, stderr, err := ipfs2fil(t, "push", "--out", out)
	if err != nil || ms.Uploads() != 0 || !strings.Contains(stderr, "USDFC/month") {
		t.Errorf("push answered no: %v, %d uploads\n%s", err, ms.Uploads(), stderr)
	}
	stdin = strings.NewReader("y\n")
	_, stderr, err = ipfs2fil(t, "push", "--out", out)
	if err != nil || ms.Uploads() == 0 {
		t.Errorf("push answered yes: %v, %d uploads", err, ms.Uploads())
	}
	if n := strings.Count(stderr, " stored\n"); n != ms.Uploads() {
		t.Errorf("push logged %d stored pieces for %d uploads:\n%s", n, ms.Uploads(), stderr)
	}
}

func TestNoArchiveYet(t *testing.T) {
	useMemStore(t, 3)
	out := t.TempDir()
	for _, cmd := range []string{"push", "status", "verify"} {
		_, _, err := ipfs2fil(t, cmd, "--out", out)
		if err == nil || !strings.Contains(err.Error(), "ipfs2fil pack") {
			t.Errorf("%s with no archive: %v", cmd, err)
		}
	}
}

// treeState sums every path, size, mode and mtime under dir.
func treeState(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %v %d\n", p, fi.Size(), fi.Mode(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(b.String())))
}

func TestUpNeedsKeyBeforePacking(t *testing.T) {
	t.Setenv("IPFS2FIL_KEY_FILE", "")
	t.Setenv("IPFS2FIL_PRIVATE_KEY", "")
	out := filepath.Join(t.TempDir(), "archive")
	_, _, err := ipfs2fil(t, "up", "--repo", fixture(t, "v11"), "--out", out)
	if err == nil || !strings.Contains(err.Error(), "IPFS2FIL_PRIVATE_KEY") {
		t.Errorf("up with no key: %v", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("up packed before checking the key")
	}
}
