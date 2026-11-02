package explorer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/gitarchive"
)

func needNodeGit(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	return node
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_COMMITTER_DATE=1700000000 +0000",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimRight(string(out), "\n")
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// The Git viewer's pack reader, run under node against `explore`'s content
// endpoint: every commit's tree, listed from a full pack and a thin pack
// whose deltas name bases in the full one, matches git ls-tree, and every
// blob reads and matches its id.
func TestGitPackReader(t *testing.T) {
	node := needNodeGit(t)
	repo := t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	big := strings.Repeat("a line of text that repeats in every version of the file\n", 3000)
	for i := 0; i < 5; i++ {
		writeFile(t, filepath.Join(repo, "big.txt"), big+fmt.Sprintf("version %d\n", i), 0o644)
		writeFile(t, filepath.Join(repo, "src", "deep", "dir", fmt.Sprintf("f%d.go", i)), fmt.Sprintf("package f%d\n", i), 0o644)
		writeFile(t, filepath.Join(repo, "run.sh"), fmt.Sprintf("#!/bin/sh\necho %d\n", i), 0o755)
		gitIn(t, repo, "add", "-A")
		gitIn(t, repo, "commit", "-q", "-m", fmt.Sprintf("commit %d", i))
	}
	os.Symlink("big.txt", filepath.Join(repo, "link"))
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "a link")

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pieces"), 0o755)
	snap := func(name string, prev *gitarchive.Result) *gitarchive.Result {
		o := gitarchive.Options{TempDir: t.TempDir()}
		if prev != nil {
			o.Prev, o.PrevRoot = prev.Record, prev.Root
		}
		res, err := gitarchive.WriteCAR(filepath.Join(dir, "pieces", name), func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
			return gitarchive.Archive(context.Background(), repo, o, put)
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	full := snap("0.car", nil)
	// edits to big.txt: the thin pack deltas them against the full pack's blobs
	for i := 5; i < 8; i++ {
		writeFile(t, filepath.Join(repo, "big.txt"), big+fmt.Sprintf("version %d\n", i), 0o644)
		gitIn(t, repo, "commit", "-q", "-am", fmt.Sprintf("commit %d", i))
	}
	thin := snap("1.car", full)
	if !thin.Record.Packs[0].Thin {
		t.Fatal("second snapshot is not thin")
	}

	s := newCARBlocks([]string{dir})
	defer s.close()
	mux := http.NewServeMux()
	s.handle(mux, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	type pk struct {
		Pack string `json:"pack"`
		Idx  string `json:"idx"`
	}
	url := func(c cid.Cid, name string) string { return srv.URL + "/_i2f/content/" + c.String() + "?name=" + name }
	var packs []pk
	for _, r := range []*gitarchive.Result{full, thin} {
		for _, p := range r.Record.Packs {
			packs = append(packs, pk{url(p.Pack.Cid, "x.pack"), url(p.Idx.Cid, "x.idx")})
		}
	}
	type cm struct {
		ID   string `json:"id"`
		Tree string `json:"tree"`
	}
	var commits []cm
	for _, l := range strings.Split(gitIn(t, repo, "log", "--format=%H %T"), "\n") {
		f := strings.Fields(l)
		commits = append(commits, cm{f[0], f[1]})
	}
	arg, _ := json.Marshal(map[string]any{"packs": packs, "format": "sha1", "commits": commits, "cold": commits[0].Tree})
	cmd := exec.Command(node, filepath.Join("testdata", "gitpack_harness.js"), filepath.Join("assets", "gitpack.js"), "walk", string(arg))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var want []string
	for _, c := range commits {
		want = append(want, "== "+c.ID, gitIn(t, repo, "ls-tree", "-r", "-t", "--full-tree", c.ID))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var got []string
	var stats struct {
		Requests, Fetched, Objects, Deltas, CrossPack, Checked int
	}
	var cold struct{ Requests, Fetched, IdxBytes int }
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "STATS "):
			json.Unmarshal([]byte(l[6:]), &stats)
		case strings.HasPrefix(l, "COLD "):
			json.Unmarshal([]byte(l[5:]), &cold)
		default:
			got = append(got, l)
		}
	}
	if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
		t.Fatalf("trees differ from git ls-tree:\n%s\n--- want\n%s", g, w)
	}
	if stats.CrossPack == 0 || stats.Deltas == 0 {
		t.Errorf("no deltas across packs resolved: %+v", stats)
	}
	if stats.Checked == 0 {
		t.Errorf("nothing checked: %+v", stats)
	}
	// One folder from cold: the two idx files and one chunk of a pack (the
	// root tree and its delta bases sit together).
	if cold.Requests > 4 {
		t.Errorf("showing one folder took %d requests (%d bytes)", cold.Requests, cold.Fetched)
	}
	t.Logf("walk: %+v; one folder cold: %+v", stats, cold)
}

// Hostile packs fail with a reason and bounded work: ids that do not
// match, zlib bombs, delta cycles and deep chains, copies outside the
// base, missing bases, chains that build too much, servers without Range,
// damaged trees and idx files.
func TestGitPackReaderHostile(t *testing.T) {
	node := needNodeGit(t)
	cmd := exec.Command(node, filepath.Join("testdata", "gitpack_harness.js"), filepath.Join("assets", "gitpack.js"), "hostile")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var res []struct {
		Name, Res string
		Requests  int
		Objects   int
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err, string(out))
	}
	want := map[string]string{
		"good":           "ok blob 12",
		"wrong-id":       "does not match its id",
		"bomb":           "inflates to more than its header says",
		"huge":           "too large to read here",
		"ref-cycle":      "delta chain is longer than 1024",
		"deep-chain":     "delta chain is longer than 1024",
		"copy-out":       "copies from outside its base",
		"no-base":        "in none of this archive's packs",
		"work":           "builds more than 512 MB",
		"no-range-small": "ok blob 12",
		"no-range-large": "does not send byte ranges",
		"bad-tree":       "is damaged",
		"idx-checksum":   "checksum mismatch",
	}
	for _, r := range res {
		w, ok := want[r.Name]
		if !ok {
			continue
		}
		delete(want, r.Name)
		if !strings.Contains(r.Res, w) {
			t.Errorf("%s: %s; want %q", r.Name, r.Res, w)
		}
		if r.Requests > 3 {
			t.Errorf("%s: %d requests", r.Name, r.Requests)
		}
	}
	for n := range want {
		t.Errorf("case %s did not run", n)
	}
}

// TestGitPackReaderRealRepo measures the reader on a real repo, named by
// $GITPACK_REPO (a bare clone, such as github.com/ipfs/kubo): a full
// snapshot at $GITPACK_OLD (a commit, default a year back), then a thin one
// at the repo's refs. It logs what showing a folder, a file and every file
// costs, in requests and bytes.
func TestGitPackReaderRealRepo(t *testing.T) {
	src := os.Getenv("GITPACK_REPO")
	if src == "" {
		t.Skip("set GITPACK_REPO to a bare clone")
	}
	node := needNodeGit(t)
	repo := filepath.Join(t.TempDir(), "r.git")
	gitIn(t, filepath.Dir(repo), "clone", "-q", "--mirror", src, repo)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	old := os.Getenv("GITPACK_OLD")
	if old == "" {
		old = gitIn(t, repo, "rev-list", "-1", "--before=1 year ago", "HEAD")
	}
	for _, r := range strings.Split(gitIn(t, repo, "for-each-ref", "--format=%(refname)"), "\n") {
		gitIn(t, repo, "update-ref", "-d", r)
	}
	gitIn(t, repo, "update-ref", "refs/heads/master", old)
	gitIn(t, repo, "symbolic-ref", "HEAD", "refs/heads/master")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pieces"), 0o755)
	snap := func(name string, prev *gitarchive.Result) *gitarchive.Result {
		o := gitarchive.Options{TempDir: t.TempDir(), NoConfig: true}
		if prev != nil {
			o.Prev, o.PrevRoot = prev.Record, prev.Root
		}
		res, err := gitarchive.WriteCAR(filepath.Join(dir, "pieces", name), func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
			return gitarchive.Archive(context.Background(), repo, o, put)
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d objects, %d bytes of pack", name, res.Objects, res.PackBytes)
		return res
	}
	full := snap("0.car", nil)
	gitIn(t, repo, "fetch", "-q", src, "+refs/*:refs/*")
	thin := snap("1.car", full)

	s := newCARBlocks([]string{dir})
	defer s.close()
	mux := http.NewServeMux()
	s.handle(mux, "")
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var packs []map[string]string
	for _, r := range []*gitarchive.Result{full, thin} {
		for _, p := range r.Record.Packs {
			packs = append(packs, map[string]string{"pack": srv.URL + "/_i2f/content/" + p.Pack.String() + "?name=x.pack", "idx": srv.URL + "/_i2f/content/" + p.Idx.String() + "?name=x.idx"})
		}
	}
	tree := func(c string) string { return gitIn(t, repo, "rev-parse", c+"^{tree}") }
	readme := gitIn(t, repo, "rev-parse", head+":README.md")
	steps := [][2]string{{"tree", tree(head)}, {"read", readme}, {"tree", tree(old)}, {"walk", tree(head)}, {"walk", tree(old)}}
	arg, _ := json.Marshal(map[string]any{"packs": packs, "format": "sha1", "steps": steps})
	cmd := exec.Command(node, filepath.Join("testdata", "gitpack_harness.js"), filepath.Join("assets", "gitpack.js"), "probe", string(arg))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", out)
}
