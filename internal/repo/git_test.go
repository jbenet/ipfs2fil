package repo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/gitarchive"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commitFile(t *testing.T, dir, name, body string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-q", "-m", "add "+name)
}

func TestGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := GitDir
	GitDir = t.TempDir()
	defer func() { GitDir = old }()
	dir := filepath.Join(t.TempDir(), "project")
	os.Mkdir(dir, 0o755)
	gitT(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "a.txt", "a\n")

	open := func() *gitRepo {
		r, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind() != KindGit {
			t.Fatalf("kind %s", r.Kind())
		}
		return r.(*gitRepo)
	}
	r := open()
	pins, err := r.Pins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Name != "project" {
		t.Fatalf("pins %+v", pins)
	}
	if !strings.Contains(Describe(r), "git repo (sha1), 1 refs, 1 snapshot") {
		t.Fatalf("describe %q", Describe(r))
	}
	first := pins[0].Root
	n := 0
	r.ForEachBlock(context.Background(), func(mh multihash.Multihash, size int) error { n++; return nil })
	if n < 4 {
		t.Fatalf("%d blocks", n)
	}

	// Unchanged: the same root, no new CAR.
	r = open()
	if p, _ := r.Pins(context.Background()); p[0].Root != first {
		t.Fatal("unchanged repo got a new root")
	}
	// A new commit: a delta on top, both CARs read.
	commitFile(t, dir, "b.txt", "b\n")
	r = open()
	p, _ := r.Pins(context.Background())
	if p[0].Root == first {
		t.Fatal("new commit did not make a new snapshot")
	}
	ch, err := gitarchive.Chain(r.verified, p[0].Root)
	if err != nil || len(ch) != 2 || ch[0].Root != first {
		t.Fatalf("chain %v %v", len(ch), err)
	}
	cars, _ := filepath.Glob(filepath.Join(r.archiveDir(), "*.car"))
	if len(cars) != 2 {
		t.Fatalf("cars %v", cars)
	}
	// It restores.
	if _, err := gitarchive.Restore(context.Background(), r.verified, p[0].Root, filepath.Join(t.TempDir(), "r.git"), gitarchive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
}

// An IPFS repo kept in a git checkout is still read as an IPFS repo; a bare
// git repo, which also has a config file, is git.
func TestIPFSRepoInGitCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(extractFixture(t, filepath.Join(fixtureDir, "v18.tar.gz")), "repo")
	gitT(t, dir, "init", "-q")
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Kind() != KindFSRepo {
		t.Fatalf("an fs-repo in a git checkout opened as %s", r.Kind())
	}
	if got, err := Discover(dir); err != nil || got != dir {
		t.Fatalf("discover: %q %v", got, err)
	}
	bare := filepath.Join(t.TempDir(), "b.git")
	gitT(t, t.TempDir(), "init", "-q", "--bare", bare)
	if b, err := Open(bare); err != nil || b.Kind() != KindGit {
		t.Fatalf("bare repo: %v", err)
	}
}

// A folder of git repos is one repo of kind git whose pin is a set record;
// a stray .car in a working tree does not make it a folder of CARs.
func TestGitFolder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := GitDir
	GitDir = t.TempDir()
	defer func() { GitDir = old }()
	src := filepath.Join(t.TempDir(), "src")
	a := filepath.Join(src, "a")
	os.MkdirAll(a, 0o755)
	gitT(t, a, "init", "-q", "-b", "main")
	commitFile(t, a, "x.txt", "x\n")
	gitT(t, src, "clone", "-q", a, "b")
	commitFile(t, filepath.Join(src, "b"), "y.txt", "y\n")
	os.WriteFile(filepath.Join(a, "test.car"), []byte("not really"), 0o644)

	if p, err := Discover(src); err != nil || p != src {
		t.Fatalf("discover %q %v", p, err)
	}
	r, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != KindGit || !strings.Contains(Describe(r), "folder of 2 git repos, 1 snapshot") {
		t.Fatalf("%s %q", r.Kind(), Describe(r))
	}
	pins, err := r.Pins(context.Background())
	if err != nil || len(pins) != 1 || pins[0].Name != "src" {
		t.Fatalf("pins %+v %v", pins, err)
	}
	g := r.(*gitRepo)
	set, err := gitarchive.LoadSet(g.verified, pins[0].Root)
	if err != nil || len(set.Repos) != 2 {
		t.Fatalf("set %+v %v", set, err)
	}
	b, err := gitarchive.LoadRecord(g.verified, set.Repos[1].Record.Cid)
	if err != nil || len(b.Bases) != 1 {
		t.Fatalf("fork not packed against a: %+v %v", b, err)
	}
	// Unchanged: same process reuses it; a commit makes a new set.
	if r2, _ := Open(src); r2 != r {
		t.Fatal("unchanged folder reopened")
	}
	commitFile(t, a, "z.txt", "z\n")
	r3, err := Open(src)
	if err != nil || r3 == r {
		t.Fatal("changed folder not reopened", err)
	}
	p3, _ := r3.Pins(context.Background())
	if p3[0].Root == pins[0].Root || !strings.Contains(Describe(r3), "2 snapshots") {
		t.Fatalf("no new snapshot: %q", Describe(r3))
	}
	if _, err := gitarchive.RestoreSet(context.Background(), r3.(*gitRepo).verified, p3[0].Root, filepath.Join(t.TempDir(), "out"), nil, gitarchive.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	// The home folder is never archived as a folder of repos.
	t.Setenv("HOME", src)
	if _, err := Open(src); err == nil || !strings.Contains(err.Error(), "name a folder of repos") {
		t.Fatalf("home folder: %v", err)
	}
	if _, err := Discover(src); err == nil {
		t.Fatal("home folder discovered")
	}
	// An empty folder is not a git folder.
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("empty folder opened")
	}
}

func TestIsGitURL(t *testing.T) {
	for s, want := range map[string]bool{
		"git+https://example.com/x/y":     true,
		"https://example.com/x/y.git":     true,
		"https://github.com/ipfs/kubo":    true,
		"https://github.com/ipfs/kubo/":   true,
		"https://GitHub.com/ipfs/kubo":    true,
		"https://github.com/ipfs":         false, // an org page, not a repo
		"https://example.com:5001":        false, // a kubo RPC API
		"http://127.0.0.1:5001":           false,
		"https://ipfs.example.com/api/v0": false,
		"http://github.com/ipfs/kubo":     false, // http is not cloned
		"git+http://example.com/x.git":    false,
		"at://bsky.app":                   false,
		"/home/me/src/kubo.git":           false,
	} {
		if got := IsGitURL(s); got != want {
			t.Errorf("IsGitURL(%q) = %v", s, got)
		}
	}
	if _, err := Open("https://user:token@github.com/ipfs/kubo"); err == nil || strings.Contains(err.Error(), "token@") {
		t.Fatalf("token URL: %v", err)
	}
}

// A checkout of kubo's own source has a config/ folder and a blocks/
// folder; it is a git repo, not an IPFS repo (whose config is a file).
func TestKuboSourceCheckoutIsGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q")
	for _, d := range []string{"config", "blocks", "datastore"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	if !isGit(dir) {
		t.Fatal("a checkout with config/ and blocks/ folders is not taken for git")
	}
}
