package repo

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/syndtr/goleveldb/leveldb"
)

// The fixtures are made by testdata/fixtures/make.sh with real go-ipfs and
// kubo binaries; expected/ holds what that binary reported.
const fixtureDir = "../../testdata/fixtures"

func TestFixtures(t *testing.T) {
	names, err := filepath.Glob(filepath.Join(fixtureDir, "*.tar.gz"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no fixtures in %s: %v", fixtureDir, err)
	}
	for _, f := range names {
		name := strings.TrimSuffix(filepath.Base(f), ".tar.gz")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := extractFixture(t, f)
			checkFixture(t, dir, name, true)
		})
	}
}

// TestLockedDatastore opens repos whose leveldb is held by another process,
// as a running daemon would.
func TestLockedDatastore(t *testing.T) {
	for _, name := range []string{"v00", "v07", "v18"} {
		t.Run(name, func(t *testing.T) {
			dir := extractFixture(t, filepath.Join(fixtureDir, name+".tar.gz"))
			db, err := leveldb.OpenFile(filepath.Join(dir, "repo", "datastore"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			checkFixture(t, dir, name, true)
		})
	}
}

// TestRunningDaemon reads a fixture while a real daemon serves it. Set
// IPFS2FIL_KUBO=<fixture>:<ipfs binary> to run it, e.g. v18:/usr/local/bin/ipfs.
func TestRunningDaemon(t *testing.T) {
	spec := os.Getenv("IPFS2FIL_KUBO")
	name, bin, ok := strings.Cut(spec, ":")
	if !ok {
		t.Skip("set IPFS2FIL_KUBO=<fixture>:<ipfs binary> to run")
	}
	dir := extractFixture(t, filepath.Join(fixtureDir, name+".tar.gz"))
	repo := filepath.Join(dir, "repo")
	cmd := exec.Command(bin, "daemon", "--offline")
	cmd.Env = append(os.Environ(), "IPFS_PATH="+repo)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(repo, "api")); err == nil {
			break
		}
		if i == 300 {
			t.Fatal("daemon did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The daemon may write to its repo, so only the contents are checked.
	checkFixture(t, dir, name, false)
}

// checkFixture opens the fixture's repo and compares it with expected/. With
// unchanged set it also checks that reading left every file as it was.
func checkFixture(t *testing.T, dir, name string, unchanged bool) {
	ctx := context.Background()
	path := filepath.Join(dir, "repo")
	before := treeHash(t, path)

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := strconv.Atoi(name[1:3]); int(r.Version()) != want {
		t.Errorf("version %d, want %d", r.Version(), want)
	}

	// Blocks match `ipfs refs local`, and each one hashes to its key.
	got := map[string]bool{}
	err = r.ForEachBlock(ctx, func(mh multihash.Multihash, size int) error {
		data, err := r.Get(ctx, mh)
		if err != nil {
			return fmt.Errorf("get %s: %w", mh.B58String(), err)
		}
		if len(data) != size {
			return fmt.Errorf("%s: size %d, ForEachBlock said %d", mh.B58String(), len(data), size)
		}
		dm, _ := multihash.Decode(mh)
		if sum, _ := multihash.Sum(data, dm.Code, dm.Length); string(sum) != string(mh) {
			return fmt.Errorf("%s fails its hash", mh.B58String())
		}
		got[mh.B58String()] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, l := range readLines(t, filepath.Join(dir, "expected", "refs-local")) {
		want[cid.MustParse(l).Hash().B58String()] = true
	}
	diffSets(t, "blocks", got, want)

	// Pins match `ipfs pin ls` (recursive and direct).
	pins, err := r.Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pl []string
	for _, p := range pins {
		pl = append(pl, strings.TrimSpace(p.Root.String()+" "+p.Mode.String()+" "+p.Name))
	}
	sort.Strings(pl)
	if w := readLines(t, filepath.Join(dir, "expected", "pins")); strings.Join(pl, "\n") != strings.Join(w, "\n") {
		t.Errorf("pins:\n%s\nwant:\n%s", strings.Join(pl, "\n"), strings.Join(w, "\n"))
	}

	// MFS root matches `ipfs files stat --hash /`, from repo 3 on.
	mfs, ok, err := r.MFSRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if w, err := os.ReadFile(filepath.Join(dir, "expected", "mfs")); err == nil {
		if !ok || mfs.String() != strings.TrimSpace(string(w)) {
			t.Errorf("MFS root %v (%v), want %s", mfs, ok, w)
		}
	} else if ok {
		t.Errorf("MFS root %s, want none", mfs)
	}

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := treeHash(t, path); unchanged && after != before {
		t.Errorf("reading the repo changed it:\n%s", diffLines(before, after))
	}
}

func diffSets(t *testing.T, what string, got, want map[string]bool) {
	t.Helper()
	var extra, missing []string
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	if len(extra)+len(missing) > 0 {
		t.Errorf("%s: %d found, %d expected; extra %v; missing %v", what, len(got), len(want), extra, missing)
	}
}

// treeHash lists every file under dir with its size and content hash.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			fmt.Fprintf(&b, "%s/\n", rel)
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %x\n", rel, len(data), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func diffLines(a, b string) string {
	in := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		in[l]--
	}
	for _, l := range strings.Split(b, "\n") {
		in[l]++
	}
	var out []string
	for l, n := range in {
		if n < 0 {
			out = append(out, "- "+l)
		} else if n > 0 {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if l := strings.TrimSpace(s.Text()); l != "" {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func extractFixture(t *testing.T, archive string) string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return dir
		}
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Fatalf("bad path %q in %s", h.Name, archive)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
				var w *os.File
				if w, err = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
					_, err = io.Copy(w, tr)
					w.Close()
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
