package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/manifest"
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
	// Both repos' changes are in one bundle piece, which holds the blocks
	// they share once.
	if !strings.Contains(stdout, "bundle   the changes of 2 repos in 1 piece") {
		t.Errorf("pack did not bundle both repos:\n%s", stdout)
	}
	bundles := map[string]bool{}
	for _, m := range []*manifest.Manifest{m1, m2} {
		for _, p := range m.Pieces {
			if p.Part == manifest.PartBundle {
				bundles[p.PieceCID] = true
			}
		}
	}
	if len(bundles) != 1 {
		t.Errorf("bundles %v, want one for both", bundles)
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

	// verify and status find listed repos without --out: all, or one by name.
	stdout, _, err = ipfs2fil(t, "verify", "--home", home, "--ipni", "")
	if err != nil || !strings.Contains(stdout, "== first (1 of 2)") || !strings.Contains(stdout, "== second (2 of 2)") || strings.Count(stdout, ", 0 bad") != 2 {
		t.Errorf("verify of every listed repo: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "verify", "--home", home, "--repo", "second", "--ipni", "")
	if err != nil || strings.Contains(stdout, "==") || strings.Count(stdout, ", 0 bad") != 1 {
		t.Errorf("verify --repo second: %v\n%s", err, stdout)
	}
	if _, _, err := ipfs2fil(t, "verify", "--home", home, "--repo", "third", "--ipni", ""); err == nil || !strings.Contains(err.Error(), "first, second") {
		t.Errorf("verify of a repo not in the list: %v", err)
	}
	stdout, _, err = ipfs2fil(t, "status", "--home", home, "--repo", "first")
	if err != nil || !strings.Contains(stdout, m1.Root) {
		t.Errorf("status --repo first: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "status", "--home", home)
	if err != nil || !strings.Contains(stdout, "data sets shared by every repo") {
		t.Errorf("status with listed repos and no --out: %v\n%s", err, stdout)
	}

	stdout, _, err = ipfs2fil(t, "status", "--all", "--home", home)
	if err != nil || !strings.Contains(stdout, "copies   3 data sets shared by every repo") ||
		strings.Contains(stdout, "missing") || strings.Contains(stdout, "earlier") {
		t.Errorf("status --all: %v\n%s", err, stdout)
	}

	// A piece no current archive uses, as an earlier snapshot leaves.
	old := bytes.Repeat([]byte("an earlier snapshot's piece "), 20)
	pc, n, err := car.PieceCID(bytes.NewReader(old))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := ms.DataSets(context.Background(), c.Key, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, ds := range sets {
		if _, err := ms.Upload(context.Background(), ds, pc.String(), bytes.NewReader(old), n, nil); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, err = ipfs2fil(t, "status", "--all", "--home", home)
	if err != nil || !strings.Contains(stdout, "earlier  1 pieces (560 B)") || strings.Contains(stdout, "missing") {
		t.Errorf("status --all with an earlier piece: %v\n%s", err, stdout)
	}
	for _, r := range []string{"first", "second"} {
		if !strings.Contains(stdout, r+" ") {
			t.Errorf("status --all lacks %s:\n%s", r, stdout)
		}
	}
	stdout, _, err = ipfs2fil(t, "status", "--all", "--home", home, "--json")
	if err != nil || !strings.Contains(stdout, `"stored": `) {
		t.Errorf("status --all --json: %v\n%s", err, stdout)
	}
	stdout, _, err = ipfs2fil(t, "repos", "ls", "--home", home)
	if err != nil || !strings.Contains(stdout, "2 repos (2 on this machine)") {
		t.Errorf("repos ls: %v\n%s", err, stdout)
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
	// Packed in one go, the two share a bundle piece. Changed afterwards,
	// the later repo shares the earlier one's pieces instead.
	f := fixture(t, "v17")
	if _, _, err := ipfs2fil(t, "repos", "rm", "v17", "--home", home); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ipfs2fil(t, "repos", "add", f, "--name", "later", "--home", home); err != nil {
		t.Fatal(err)
	}
	if out, _, err := ipfs2fil(t, "pack", "--all", "--home", home); err != nil || !strings.Contains(out, "already in earlier repos' pieces") {
		t.Fatalf("pack: %v\n%s", err, out)
	}
	_, _, err := ipfs2fil(t, "push", "--out", filepath.Join(home, "archives", "later"), "--yes")
	if err == nil || !strings.Contains(err.Error(), "push --all") {
		t.Errorf("push of the later repo alone: %v", err)
	}
	// The first repo holds blocks the later one relies on.
	if _, _, err := ipfs2fil(t, "repos", "rm", "v18", "--home", home); err == nil || !strings.Contains(err.Error(), "later") {
		t.Errorf("rm of a repo others rely on: %v", err)
	}
}

// TestUpAllPushesWhatPacked: a repo that fails to pack does not stop the
// others from being pushed, and a re-run once it is fixed picks up the rest.
func TestUpAllPushesWhatPacked(t *testing.T) {
	ms := useMemStore(t, 5)
	home := t.TempDir()
	a, b := fixture(t, "v18"), fixture(t, "v11")
	for name, r := range map[string]string{"first": a, "second": b} {
		if _, _, err := ipfs2fil(t, "repos", "add", r, "--name", name, "--home", home); err != nil {
			t.Fatal(err)
		}
	}
	version, err := os.ReadFile(filepath.Join(a, "version"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "version"), []byte("999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := ipfs2fil(t, "up", "--all", "--home", home, "--yes")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 repos did not pack") || !strings.Contains(err.Error(), "first") {
		t.Fatalf("up --all with a broken repo: %v\n%s", err, stdout)
	}
	c, err := collection.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := c.Manifest(c.Repos[c.Find("second")])
	if m == nil || len(m.Copies) != 3 {
		t.Fatalf("the repo that packed was not pushed: %+v", m)
	}
	uploads := ms.Uploads()
	if uploads == 0 {
		t.Fatal("nothing uploaded")
	}

	os.WriteFile(filepath.Join(a, "version"), version, 0o644)
	stdout, _, err = ipfs2fil(t, "up", "--all", "--home", home, "--yes")
	if err != nil {
		t.Fatalf("up --all once fixed: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "unchanged since the last pack") || !strings.Contains(stdout, "second: all") {
		t.Errorf("the re-run redid the repo already stored:\n%s", stdout)
	}
	m, _ = c.Manifest(c.Repos[c.Find("first")])
	if m == nil || len(m.Copies) != 3 {
		t.Fatalf("the fixed repo was not pushed: %+v", m)
	}
}

func TestDefaultName(t *testing.T) {
	for in, want := range map[string]string{"/home/x/.ipfs": "x", "/Users/ada/.ipfs": "ada", "/.ipfs": "ipfs", "/Volumes/old-laptop/ipfs": "old-laptop",
		"/data/my repo": "my-repo", "/x/kubo-0.43": "kubo-0.43", "/srv/go-ipfs": "go-ipfs",
		"http://nas.local:5001": "nas.local", "http://user:pw@10.0.0.2:5001/api/v0": "10.0.0.2", "/backups/2021.car": "2021.car",
		"at://bsky.app": "bsky-bsky.app", "@Alice.bsky.social": "bsky-alice.bsky.social", "did:plc:abc123": "bsky-did-plc-abc123"} {
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
		t.Errorf("server uploaded %d pieces, want 3 (its bundle piece in 3 copies)", got)
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

// TestReposGitCatalog: the catalog is a git repository. Each machine clones
// it, pushes its own changes and pulls the other's; --catalog is given once.
func TestReposGitCatalog(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	ms := useMemStore(t, 5)
	remote := filepath.Join(t.TempDir(), "catalog.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	laptop := []string{"--home", t.TempDir(), "--host", "laptop"}
	server := []string{"--home", t.TempDir(), "--host", "server"}
	on := func(machine []string, args ...string) (string, error) {
		t.Helper()
		out, _, err := ipfs2fil(t, append(args, machine...)...)
		return out, err
	}

	if _, err := on(laptop, "repos", "add", fixture(t, "v18"), "--name", "photos", "--catalog", remote); err != nil {
		t.Fatal(err)
	}
	if out, err := on(laptop, "up", "--all", "--yes"); err != nil { // catalog remembered
		t.Fatalf("laptop up: %v\n%s", err, out)
	}
	before := ms.Uploads()
	if _, err := on(server, "repos", "add", fixture(t, "v17"), "--name", "backup", "--catalog", remote); err != nil {
		t.Fatal(err)
	}
	out, err := on(server, "up", "--all", "--yes")
	if err != nil || !strings.Contains(out, "already in earlier repos' pieces") {
		t.Fatalf("server up: %v\n%s", err, out)
	}
	if got := ms.Uploads() - before; got != 3 {
		t.Errorf("server uploaded %d pieces, want 3 (its bundle piece in 3 copies)", got)
	}
	// The laptop pulls the server's repo.
	out, err = on(laptop, "status", "--all")
	if err != nil || !strings.Contains(out, "server/backup") || !strings.Contains(out, "laptop/photos") {
		t.Errorf("laptop status --all: %v\n%s", err, out)
	}
	log, err := exec.Command("git", "--git-dir", remote, "log", "--format=%s", "--all").CombinedOutput()
	if err != nil || !strings.Contains(string(log), "laptop: add photos") || !strings.Contains(string(log), "server: push") {
		t.Errorf("catalog history: %v\n%s", err, log)
	}
}

// TestReposChainCatalog: with no --catalog, each machine reads the others'
// repos from the shared data sets on chain. The second machine stores none of
// the blocks the first already stored, each sees both repos, and nothing but
// pieces is shared.
func TestReposChainCatalog(t *testing.T) {
	ms := useMemStore(t, 5)
	laptopHome, serverHome := t.TempDir(), t.TempDir()
	laptop := []string{"--home", laptopHome, "--host", "laptop"}
	server := []string{"--home", serverHome, "--host", "server"}
	on := func(machine []string, args ...string) (string, string, error) {
		t.Helper()
		return ipfs2fil(t, append(args, machine...)...)
	}

	if _, _, err := on(laptop, "repos", "add", fixture(t, "v18"), "--name", "photos"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := on(server, "repos", "add", fixture(t, "v17"), "--name", "backup"); err != nil {
		t.Fatal(err)
	}
	if out, _, err := on(laptop, "up", "--all", "--yes"); err != nil {
		t.Fatalf("laptop up: %v\n%s", err, out)
	}
	afterLaptop := ms.Uploads()
	out, errOut, err := on(server, "up", "--all", "--yes")
	if err != nil || !strings.Contains(out, "already in earlier repos' pieces") || strings.Contains(errOut, "warning") {
		t.Fatalf("server up: %v\n%s\n%s", err, out, errOut)
	}
	if got := ms.Uploads() - afterLaptop; got != 3 {
		t.Errorf("server uploaded %d pieces, want 3 (its bundle piece in 3 copies)", got)
	}
	afterServer := ms.Uploads()
	if out, _, err := on(server, "up", "--all", "--yes"); err != nil || !strings.Contains(out, "nothing to upload") {
		t.Errorf("server again: %v\n%s", err, out)
	}
	if ms.Uploads() != afterServer {
		t.Errorf("an unchanged re-run uploaded %d pieces", ms.Uploads()-afterServer)
	}

	for _, m := range [][]string{laptop, server} {
		out, errOut, err := on(m, "status", "--all")
		if err != nil || !strings.Contains(out, "laptop/photos") || !strings.Contains(out, "server/backup") ||
			!strings.Contains(out, "2 repos") || strings.Contains(out, "missing") || strings.Contains(errOut, "warning") {
			t.Errorf("status --all: %v\n%s\n%s", err, out, errOut)
		}
	}
	if _, _, err := on(laptop, "repos", "rm", "photos"); err == nil || !strings.Contains(err.Error(), "server/backup") {
		t.Errorf("rm of a repo another machine relies on: %v", err)
	}
	// The names are sealed: nothing in the pieces says "photos" or "backup".
	for _, dir := range []string{filepath.Join(laptopHome, "archives", "photos", "pieces"), filepath.Join(serverHome, "archives", "backup", "pieces")} {
		if out, _ := exec.Command("grep", "-rlaE", "photos|backup", dir).Output(); len(out) > 0 {
			t.Errorf("a piece names the repo: %s", out)
		}
	}
	// A machine with another wallet key reads nothing.
	walletSecret = func() ([]byte, error) { return []byte("another wallet"), nil }
	other := []string{"--home", t.TempDir(), "--host", "other"}
	out, errOut, err = on(other, "status", "--all")
	if err == nil || strings.Contains(out, "photos") {
		t.Errorf("another wallet's status --all: %v\n%s\n%s", err, out, errOut)
	}
}

// TestHostRenamedAndCopiesLost: a run leaves the archive's manifest without
// its copies, and a Mac's host name changes between runs. A re-run records
// the copies without uploading and the explorer shows them, and the repo
// still lists once.
func TestHostRenamedAndCopiesLost(t *testing.T) {
	ms := useMemStore(t, 5)
	home := t.TempDir()
	on := func(host string, args ...string) (string, error) {
		t.Helper()
		out, _, err := ipfs2fil(t, append(args, "--home", home, "--host", host)...)
		return out, err
	}
	// backup shares the data sets, so photos adopts them once its own are
	// lost. Photos comes first: a later repo that loses its copies shares
	// blocks earlier repos store, so its archive changes.
	for _, nf := range [][2]string{{"photos", "v18"}, {"backup", "v17"}} {
		if _, err := on("unknownc2de", "repos", "add", fixture(t, nf[1]), "--name", nf[0]); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := on("unknownc2de", "up", "--all", "--yes"); err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}
	uploads := ms.Uploads()
	mpath := filepath.Join(home, "archives", "photos", archive.ManifestFile)
	m, err := manifest.Read(mpath)
	if err != nil {
		t.Fatal(err)
	}
	m.Copies = nil
	if err := m.Write(mpath); err != nil {
		t.Fatal(err)
	}
	if out, err := on("unknownc2de", "up", "--all", "--yes"); err != nil || ms.Uploads() != uploads {
		t.Fatalf("up again: %v, %d uploads\n%s", err, ms.Uploads()-uploads, out)
	}
	if m, err = manifest.Read(mpath); err != nil || len(m.Copies) != 3 {
		t.Fatalf("copies after the re-run: %v %+v", err, m)
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != len(m.Pieces) {
			t.Errorf("copy on %s has ids for %d of %d pieces", c.Provider, len(c.PieceIDs), len(m.Pieces))
		}
	}
	js, err := os.ReadFile(filepath.Join(home, "archives", "photos", "explorer", "idx", "manifest.js"))
	if err != nil || !strings.Contains(string(js), `"copies":[`) {
		t.Errorf("the explorer does not show the copies: %v", err)
	}

	// Renamed: the records sealed under the old name are this machine's.
	if out, err := on("Synnax", "repos", "ls"); err != nil || strings.Count(out, "photos") != 1 {
		t.Errorf("repos ls after the rename lists photos other than once: %v\n%s", err, out)
	}
	if out, err := on("Synnax", "up", "--all", "--yes"); err != nil {
		t.Fatalf("up after the rename: %v\n%s", err, out)
	}
	if out, err := on("Synnax", "repos", "ls"); err != nil || strings.Count(out, "photos") != 1 {
		t.Errorf("repos ls after up under the new name lists photos other than once: %v\n%s", err, out)
	}
}

// TestAdoptCopies: an archive never pushed takes the data sets of an archive
// with the same key, on this machine first, else on another machine.
func TestAdoptCopies(t *testing.T) {
	remote := map[string][]manifest.Copy{"k": {{Provider: "4", DataSetID: "46739", Service: "chain", PieceIDs: map[int]string{0: "3"}}}}
	a := &listedArchive{key: "k", m: &manifest.Manifest{}}
	adoptCopies([]*listedArchive{a}, remote)
	if len(a.m.Copies) != 1 || a.m.Copies[0].DataSetID != "46739" || a.m.Copies[0].Service != "" || len(a.m.Copies[0].PieceIDs) != 0 {
		t.Errorf("from another machine: %+v", a.m.Copies)
	}
	local := &listedArchive{key: "k", m: &manifest.Manifest{Copies: []manifest.Copy{{Provider: "2", DataSetID: "7", Service: "foc-fwss"}}}}
	b := &listedArchive{key: "k", m: &manifest.Manifest{}}
	adoptCopies([]*listedArchive{local, b}, remote)
	if len(b.m.Copies) != 1 || b.m.Copies[0].DataSetID != "7" {
		t.Errorf("from this machine: %+v", b.m.Copies)
	}
}

// TestExploreAfterUpAll: explore with no flags finds the archives 'up --all'
// packed into --home, all of them or the one named.
func TestExploreAfterUpAll(t *testing.T) {
	useMemStore(t, 5)
	home := t.TempDir()
	repos := map[string]string{"v18": fixture(t, "v18"), "v17": fixture(t, "v17")}
	t.Setenv(collection.EnvHome, home)
	t.Chdir(t.TempDir())
	if _, _, err := ipfs2fil(t, "explore", "--serve=false"); err == nil || !strings.Contains(err.Error(), "ipfs2fil up") {
		t.Errorf("explore with nothing packed: %v", err)
	}
	for _, f := range []string{"v18", "v17"} {
		if _, _, err := ipfs2fil(t, "repos", "add", repos[f], "--name", f); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ipfs2fil(t, "explore", "--serve=false"); err == nil || !strings.Contains(err.Error(), "up --all") {
		t.Errorf("explore before packing: %v", err)
	}
	if _, _, err := ipfs2fil(t, "pack", "--all"); err != nil {
		t.Fatal(err)
	}
	out, _, err := ipfs2fil(t, "explore", "--serve=false")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"v18", "v17"} {
		if !strings.Contains(out, filepath.Join(home, "archives", f, "explorer")) {
			t.Errorf("explore did not refresh %s:\n%s", f, out)
		}
	}
	out, _, err = ipfs2fil(t, "explore", "--serve=false", "v17")
	if err != nil || strings.Contains(out, "v18") {
		t.Errorf("explore v17: %v\n%s", err, out)
	}
	out, _, err = ipfs2fil(t, "explore", "--serve=false", "--repo", repos["v18"])
	if err != nil || strings.Contains(out, "v17") || !strings.Contains(out, "v18") {
		t.Errorf("explore --repo <path>: %v\n%s", err, out)
	}
	if _, _, err := ipfs2fil(t, "explore", "--serve=false", "nope"); err == nil || !strings.Contains(err.Error(), "v18, v17") {
		t.Errorf("explore of an unlisted repo: %v", err)
	}
}

// TestChainCatalogReadsBundles: two repos packed on one machine share one
// bundle piece; another machine reads both from chain, and each exports
// from Filecoin exactly as the first machine's own archive does.
func TestChainCatalogReadsBundles(t *testing.T) {
	ms := useMemStore(t, 5)
	laptop := []string{"--home", t.TempDir(), "--host", "laptop"}
	server := []string{"--home", t.TempDir(), "--host", "server"}
	on := func(machine []string, args ...string) (string, string, error) {
		t.Helper()
		return ipfs2fil(t, append(args, machine...)...)
	}
	for name, f := range map[string]string{"photos": "v18", "notes": "v11"} {
		if _, _, err := on(laptop, "repos", "add", fixture(t, f), "--name", name); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := on(server, "repos", "add", fixture(t, "v17"), "--name", "backup"); err != nil {
		t.Fatal(err)
	}
	out, _, err := on(laptop, "up", "--all", "--yes")
	if err != nil || !strings.Contains(out, "bundle   the changes of 2 repos in 1 piece") {
		t.Fatalf("laptop up: %v\n%s", err, out)
	}
	if ms.Uploads() != 3 {
		t.Errorf("laptop uploaded %d pieces, want 3 (one bundle in 3 copies)", ms.Uploads())
	}
	out, errOut, err := on(server, "status", "--all")
	if err != nil || !strings.Contains(out, "laptop/photos") || !strings.Contains(out, "laptop/notes") ||
		strings.Contains(out, "missing") || strings.Contains(errOut, "warning") {
		t.Fatalf("server status --all: %v\n%s\n%s", err, out, errOut)
	}
	for _, name := range []string{"photos", "notes"} {
		mine, theirs := filepath.Join(t.TempDir(), "x"), filepath.Join(t.TempDir(), "x")
		if out, _, err := on(laptop, "export", "--repo", name, "--to", mine); err != nil {
			t.Fatalf("laptop export %s: %v\n%s", name, err, out)
		}
		if out, _, err := on(server, "export", "--repo", "laptop/"+name, "--to", theirs); err != nil {
			t.Fatalf("server export %s: %v\n%s", name, err, out)
		}
		for _, f := range []string{"blocks.car", "pins.json"} {
			a, _ := os.ReadFile(filepath.Join(mine, f))
			b, err := os.ReadFile(filepath.Join(theirs, f))
			if err != nil || len(a) == 0 || !bytes.Equal(a, b) {
				t.Errorf("%s: %s differs between the machines' exports (%v)", name, f, err)
			}
		}
	}
}
