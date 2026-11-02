package gitarchive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// putLFS stores body in the repo's LFS store under name (its oid unless
// given), as git-lfs does.
func putLFS(t *testing.T, gitDir, body, name string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	oid := hex.EncodeToString(sum[:])
	if name == "" {
		name = oid
	}
	write(t, filepath.Join(gitDir, "lfs", "objects", name[:2], name[2:4], name), body)
	return oid
}

func TestLFSObjects(t *testing.T) {
	needGit(t)
	dir := newRepo(t, "")
	gd := filepath.Join(dir, ".git")
	a := putLFS(t, gd, "big video bytes", "")
	b := putLFS(t, gd, strings.Repeat("dataset ", 300000), "") // over one leaf
	bad := putLFS(t, gd, "tampered", strings.Repeat("ab", 32))
	write(t, filepath.Join(gd, "lfs", "objects", "tmp", "partial"), "junk")
	write(t, filepath.Join(gd, "lfs", "incomplete", "x"), "junk")

	bs := NewBlocks()
	res := run(t, dir, Options{}, bs)
	if res.Record.LFSObjects != 2 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "corrupt") {
		t.Fatalf("lfs %d objects, notes %v (bad %s)", res.Record.LFSObjects, res.Notes, bad)
	}

	// a new object with no new commits is a new snapshot of just it
	c := putLFS(t, gd, "added later", "")
	d := run(t, dir, Options{Prev: res.Record, PrevRoot: res.Root, Get: bs.Get, Have: bs.Has}, bs)
	if d.Unchanged || d.Record.LFSObjects != 1 || d.Objects != 0 {
		t.Fatalf("delta: unchanged %v, %d LFS objects, %d git objects", d.Unchanged, d.Record.LFSObjects, d.Objects)
	}
	again := run(t, dir, Options{Prev: d.Record, PrevRoot: d.Root, Get: bs.Get}, bs)
	if !again.Unchanged || len(again.Notes) != 1 {
		t.Fatalf("no new objects: unchanged %v, notes %v", again.Unchanged, again.Notes)
	}

	dest := filepath.Join(t.TempDir(), "r.git")
	rr, err := Restore(t.Context(), bs.Get, d.Root, dest, RestoreOptions{})
	if err != nil || rr.LFS != 3 {
		t.Fatalf("restore: %+v %v", rr, err)
	}
	for _, oid := range []string{a, b, c} {
		want, _ := os.ReadFile(filepath.Join(gd, "lfs", "objects", oid[:2], oid[2:4], oid))
		got, err := os.ReadFile(filepath.Join(dest, "lfs", "objects", oid[:2], oid[2:4], oid))
		if err != nil || string(got) != string(want) {
			t.Fatalf("object %s: %v", oid, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "lfs", "objects", bad[:2])); err == nil {
		t.Fatal("corrupt object restored")
	}
}

// lfsRepo makes a repo whose commit has an LFS pointer, with the object in
// its LFS store.
func lfsRepo(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	sum := sha256.Sum256([]byte(body))
	oid := hex.EncodeToString(sum[:])
	write(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	write(t, filepath.Join(dir, "v.bin"), "version https://git-lfs.github.com/spec/v1\noid sha256:"+oid+"\nsize "+fmt.Sprint(len(body))+"\n")
	git(t, dir, "-c", "filter.lfs.clean=cat", "-c", "filter.lfs.required=false", "add", "-A")
	git(t, dir, "commit", "-q", "-m", "pointer")
	putLFS(t, filepath.Join(dir, ".git"), body, "")
	return dir, oid
}

// --lfs-fetch stores objects only the LFS server had; one that cannot
// fetch still stores the local objects, with a note.
func TestLFSFetch(t *testing.T) {
	needGit(t)
	if exec.Command("git", "lfs", "version").Run() != nil {
		t.Skip("git-lfs not installed")
	}
	src, oid := lfsRepo(t, "only on the server")
	clone := filepath.Join(t.TempDir(), "c")
	git(t, t.TempDir(), "-c", "filter.lfs.smudge=cat", "-c", "filter.lfs.process=", "-c", "filter.lfs.required=false", "clone", "-q", src, clone)
	git(t, clone, "remote", "set-url", "origin", "file://"+src)
	if res := run(t, clone, Options{}, NewBlocks()); res.Record.LFSObjects != 0 {
		t.Fatalf("without fetch: %d objects", res.Record.LFSObjects)
	}
	on := true
	bs := NewBlocks()
	res := run(t, clone, Options{LFSFetch: &on}, bs)
	if res.Record.LFSObjects != 1 || len(res.Notes) != 0 || res.Record.Settings["lfs.fetch"] != "true" {
		t.Fatalf("fetched: %d objects, notes %v", res.Record.LFSObjects, res.Notes)
	}
	objs, _ := readLFS(bs.Get, res.Record)
	if len(objs) != 1 || objs[0].OID != oid {
		t.Fatalf("objects %+v", objs)
	}

	broken, _ := lfsRepo(t, "lost")
	os.RemoveAll(filepath.Join(broken, ".git", "lfs"))
	putLFS(t, filepath.Join(broken, ".git"), "local one", "")
	git(t, broken, "remote", "add", "origin", "file://"+filepath.Join(broken, "nowhere"))
	res = run(t, broken, Options{LFSFetch: &on}, NewBlocks())
	if res.Record.LFSObjects != 1 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "lfs fetch") {
		t.Fatalf("failed fetch: %d objects, notes %v", res.Record.LFSObjects, res.Notes)
	}
}
