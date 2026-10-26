package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/collection"
)

// TestReposAll: two repos in a list -> up --all -> one set of data sets,
// shared blocks stored once -> a re-run uploads nothing -> status --all.
func TestReposAll(t *testing.T) {
	ms := useMemStore(t, 5)
	home := t.TempDir()
	a, b := fixture(t, "v18"), fixture(t, "v17")
	before := treeState(t, a) + treeState(t, b)
	t.Cleanup(func() {
		if after := treeState(t, a) + treeState(t, b); after != before {
			t.Errorf("a repo changed")
		}
	})

	if _, _, err := ipfs2fil(t, "up", "--all", "--home", home, "--yes"); err == nil || !strings.Contains(err.Error(), "repos add") {
		t.Errorf("up --all with no repos: %v", err)
	}
	stdout, _, err := ipfs2fil(t, "repos", "add", a, "--name", "first", "--home", home)
	if err != nil || !strings.Contains(stdout, "added    first") {
		t.Fatalf("repos add: %v\n%s", err, stdout)
	}
	if _, _, err := ipfs2fil(t, "repos", "--home", home, "add", "--repo", b, "--name", "second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ipfs2fil(t, "repos", "add", a, "--name", "again", "--home", home); err == nil || !strings.Contains(err.Error(), "already in the list") {
		t.Errorf("adding a repo twice: %v", err)
	}

	stdout, _, err = ipfs2fil(t, "up", "--all", "--home", home, "--yes")
	if err != nil {
		t.Fatalf("up --all: %v\n%s", err, stdout)
	}
	c, err := collection.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := c.Manifest(c.Repos[0])
	m2, _ := c.Manifest(c.Repos[1])
	if m1 == nil || m2 == nil || m1.Collection != c.Key || m2.Collection != c.Key {
		t.Fatalf("manifests not in the collection: %+v %+v", m1, m2)
	}
	if m1.Shared != nil || m2.Shared == nil || m2.Shared.Blocks == 0 {
		t.Fatalf("shared: first %+v, second %+v", m1.Shared, m2.Shared)
	}
	if !strings.Contains(stdout, "already in earlier repos' pieces") {
		t.Errorf("pack did not say what it shared:\n%s", stdout)
	}
	distinct := map[string]bool{}
	for _, m := range []int{0, 1} {
		mm, _ := c.Manifest(c.Repos[m])
		for _, p := range mm.Pieces {
			distinct[p.PieceCID] = true
		}
		if len(mm.Copies) != 3 {
			t.Errorf("repo %d has %d copies", m, len(mm.Copies))
		}
	}
	if got, want := ms.Uploads(), 3*len(distinct); got != want {
		t.Errorf("uploads = %d, want %d", got, want)
	}
	if ids := map[string]bool{m1.Copies[0].DataSetID: true, m2.Copies[0].DataSetID: true}; len(ids) != 1 {
		t.Errorf("repos use different data sets: %v", ids)
	}

	// Again: nothing new.
	stdout, _, err = ipfs2fil(t, "up", "--all", "--home", home, "--yes")
	if err != nil || !strings.Contains(stdout, "nothing to upload") {
		t.Errorf("second up --all: %v\n%s", err, stdout)
	}
	if got, want := ms.Uploads(), 3*len(distinct); got != want {
		t.Errorf("re-run uploaded %d more", got-want)
	}

	stdout, _, err = ipfs2fil(t, "status", "--all", "--home", home)
	if err != nil || !strings.Contains(stdout, "copies   3 data sets shared by every repo") ||
		!strings.Contains(stdout, "is stored once") || strings.Contains(stdout, "missing") {
		t.Errorf("status --all: %v\n%s", err, stdout)
	}
	for _, r := range []string{"first", "second"} {
		if !strings.Contains(stdout, r+" ") {
			t.Errorf("status --all lacks %s:\n%s", r, stdout)
		}
	}
	stdout, _, err = ipfs2fil(t, "status", "--all", "--home", home, "--json")
	if err != nil || !strings.Contains(stdout, `"stored": `) || !strings.Contains(stdout, `"sharedBytes": `) {
		t.Errorf("status --all --json: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "repos", "ls", "--home", home)
	if err != nil || !strings.Contains(stdout, "2 repos (2 on this machine)") {
		t.Errorf("repos ls: %v\n%s", err, stdout)
	}

	// The first repo holds blocks the second relies on.
	if _, _, err := ipfs2fil(t, "repos", "rm", "first", "--home", home); err == nil || !strings.Contains(err.Error(), "second") {
		t.Errorf("rm of a repo others rely on: %v", err)
	}
	if _, _, err := ipfs2fil(t, "repos", "rm", "second", "--home", home); err != nil {
		t.Errorf("rm: %v", err)
	}
	if c, _ := collection.Open(home); len(c.Repos) != 1 {
		t.Errorf("%d repos after rm", len(c.Repos))
	}
}

// TestPushNeedsSharedPieces: pushing a later repo alone, before the repo it
// shares blocks with, says which to push first.
func TestPushNeedsSharedPieces(t *testing.T) {
	useMemStore(t, 5)
	home := t.TempDir()
	for _, f := range []string{"v18", "v17"} {
		if _, _, err := ipfs2fil(t, "repos", "add", fixture(t, f), "--name", f, "--home", home); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ipfs2fil(t, "pack", "--all", "--home", home); err != nil {
		t.Fatal(err)
	}
	_, _, err := ipfs2fil(t, "push", "--out", filepath.Join(home, "archives", "v17"), "--yes")
	if err == nil || !strings.Contains(err.Error(), "push --all") {
		t.Errorf("push of the later repo alone: %v", err)
	}
}

func TestDefaultName(t *testing.T) {
	for in, want := range map[string]string{"/home/x/.ipfs": "ipfs", "/data/my repo": "my-repo", "/x/kubo-0.43": "kubo-0.43"} {
		if got := defaultName(in); got != want {
			t.Errorf("defaultName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestReposCatalog: two machines share a catalog folder. The second stores
// none of the blocks the first already stored, and each sees both repos.
func TestReposCatalog(t *testing.T) {
	ms := useMemStore(t, 5)
	catalog := t.TempDir()
	laptop := []string{"--home", t.TempDir(), "--catalog", catalog, "--host", "laptop"}
	server := []string{"--home", t.TempDir(), "--catalog", catalog, "--host", "server"}
	on := func(machine []string, args ...string) (string, error) {
		t.Helper()
		out, _, err := ipfs2fil(t, append(args, machine...)...)
		return out, err
	}

	if _, err := on(laptop, "repos", "add", fixture(t, "v18"), "--name", "photos"); err != nil {
		t.Fatal(err)
	}
	if _, err := on(server, "repos", "add", fixture(t, "v17"), "--name", "backup"); err != nil {
		t.Fatal(err)
	}
	// The server packs before the laptop has stored anything: nothing to share.
	if out, err := on(server, "pack", "--all"); err != nil || strings.Contains(out, "shared ") {
		t.Fatalf("server pack before the laptop pushed: %v\n%s", err, out)
	}
	if out, err := on(laptop, "up", "--all", "--yes"); err != nil {
		t.Fatalf("laptop up: %v\n%s", err, out)
	}
	afterLaptop := ms.Uploads()
	out, err := on(server, "up", "--all", "--yes")
	if err != nil || !strings.Contains(out, "already in earlier repos' pieces") {
		t.Fatalf("server up: %v\n%s", err, out)
	}
	if got := ms.Uploads() - afterLaptop; got != 3 {
		t.Errorf("server uploaded %d pieces, want 3 (its meta piece in 3 copies)", got)
	}

	for _, m := range [][]string{laptop, server} {
		out, err := on(m, "status", "--all")
		if err != nil || !strings.Contains(out, "laptop/photos") || !strings.Contains(out, "server/backup") ||
			!strings.Contains(out, "2 repos") || strings.Contains(out, "missing") {
			t.Errorf("status --all: %v\n%s", err, out)
		}
	}
	if _, err := on(laptop, "repos", "rm", "photos"); err == nil || !strings.Contains(err.Error(), "server/backup") {
		t.Errorf("rm of a repo another machine relies on: %v", err)
	}
}
