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
