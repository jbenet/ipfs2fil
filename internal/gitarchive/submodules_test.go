package gitarchive

import (
	"path/filepath"
	"strings"
	"testing"
)

// subRepos makes a repo with two commits and a parent that has it as a
// submodule at vendor/lib, checked out (so its repo is in .git/modules).
func subRepos(t *testing.T) (parent, lib string) {
	t.Helper()
	lib = t.TempDir()
	git(t, lib, "init", "-q", "-b", "main")
	write(t, filepath.Join(lib, "lib.go"), "package lib\n")
	git(t, lib, "add", "-A")
	git(t, lib, "commit", "-q", "-m", "lib one")
	write(t, filepath.Join(lib, "lib.go"), "package lib // two\n")
	git(t, lib, "commit", "-q", "-am", "lib two")
	parent = t.TempDir()
	git(t, parent, "init", "-q", "-b", "main")
	write(t, filepath.Join(parent, "main.go"), "package main\n")
	git(t, parent, "add", "-A")
	git(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "file://"+lib, "vendor/lib")
	git(t, parent, "commit", "-q", "-m", "add lib")
	return parent, lib
}

func TestSubmodules(t *testing.T) {
	needGit(t)
	parent, _ := subRepos(t)
	pin := git(t, filepath.Join(parent, "vendor/lib"), "rev-parse", "HEAD")

	// by default a submodule is listed, not archived
	bs := NewBlocks()
	res := run(t, parent, Options{}, bs)
	if len(res.Record.Submodules) != 1 {
		t.Fatalf("submodules %+v", res.Record.Submodules)
	}
	s := res.Record.Submodules[0]
	if s.Name != "vendor/lib" || s.Path != "vendor/lib" || s.Commit != pin || !strings.HasPrefix(s.URL, "file://") || s.Record != nil {
		t.Fatalf("submodule %+v", s)
	}

	// --submodules archives it, from .git/modules
	on := true
	bs = NewBlocks()
	res = run(t, parent, Options{Submodules: &on}, bs)
	s = res.Record.Submodules[0]
	if s.Record == nil || s.Note != "" || res.Record.Settings["submodules.archive"] != "true" {
		t.Fatalf("archived submodule %+v %v", s, res.Notes)
	}
	sub, err := LoadRecord(bs.Get, s.Record.Cid)
	if err != nil || !hasCommit(sub, pin) {
		t.Fatalf("submodule record %+v %v", sub, err)
	}
	dest := filepath.Join(t.TempDir(), "r.git")
	rr, err := Restore(t.Context(), bs.Get, res.Root, dest, RestoreOptions{Recurse: true})
	if err != nil || len(rr.Restored) != 1 || rr.Restored[0] != "modules/vendor/lib" {
		t.Fatalf("restore %+v %v", rr, err)
	}
	if got := git(t, filepath.Join(dest, "modules/vendor/lib"), "rev-parse", pin+"^{commit}"); got != pin {
		t.Fatalf("restored submodule lacks %s", pin)
	}

	// a new submodule commit: the submodule's archive builds on its last
	checkout := filepath.Join(parent, "vendor/lib")
	write(t, filepath.Join(checkout, "lib.go"), "package lib // three\n")
	git(t, checkout, "commit", "-q", "-am", "lib three")
	git(t, parent, "commit", "-q", "-am", "bump lib")
	d := run(t, parent, Options{Submodules: &on, Prev: res.Record, PrevRoot: res.Root, Get: bs.Get, Have: bs.Has}, bs)
	sub2, err := LoadRecord(bs.Get, d.Record.Submodules[0].Record.Cid)
	if err != nil || sub2.Prev == nil || sub2.Prev.Cid != s.Record.Cid || sub2.Commits != 1 {
		t.Fatalf("submodule delta %+v %v", sub2, err)
	}
	if again := run(t, parent, Options{Submodules: &on, Prev: d.Record, PrevRoot: d.Root, Get: bs.Get}, bs); !again.Unchanged {
		t.Fatal("unchanged repo and submodule made a new snapshot")
	}
	dest2 := filepath.Join(t.TempDir(), "r2.git")
	if _, err := Restore(t.Context(), bs.Get, d.Root, dest2, RestoreOptions{Recurse: true}); err != nil {
		t.Fatal(err)
	}

	// the config file can turn it on, unless the config is not trusted
	write(t, filepath.Join(parent, ConfigFile), "[submodules]\n\tarchive\n")
	git(t, parent, "add", ConfigFile)
	git(t, parent, "commit", "-q", "-m", "config")
	if c := run(t, parent, Options{}, NewBlocks()); c.Record.Submodules[0].Record == nil {
		t.Fatalf("config did not archive the submodule: %v", c.Notes)
	}
	if u := run(t, parent, Options{UntrustedConfig: true}, NewBlocks()); u.Record.Submodules[0].Record != nil || len(u.Notes) != 1 {
		t.Fatalf("untrusted config archived the submodule: %v", u.Notes)
	}
}

// A submodule that is not checked out is cloned from an allowed URL; a
// local path in .gitmodules is not followed.
func TestSubmoduleClone(t *testing.T) {
	needGit(t)
	parent, _ := subRepos(t)
	bare := filepath.Join(t.TempDir(), "p.git")
	git(t, t.TempDir(), "clone", "-q", "--bare", parent, bare) // no modules/
	on := true
	res := run(t, bare, Options{Submodules: &on}, NewBlocks())
	if s := res.Record.Submodules[0]; s.Record != nil || !strings.Contains(s.Note, "not an https URL") {
		t.Fatalf("file URL followed: %+v", s)
	}
	cloneSchemes["file"] = true
	defer delete(cloneSchemes, "file")
	res = run(t, bare, Options{Submodules: &on}, NewBlocks())
	if s := res.Record.Submodules[0]; s.Record == nil || s.Note != "" {
		t.Fatalf("not cloned: %+v %v", s, res.Notes)
	}
}

// A .gitmodules name that climbs out of modules/ is not used as a path.
func TestSubmoduleUnsafeName(t *testing.T) {
	needGit(t)
	parent, _ := subRepos(t)
	git(t, parent, "config", "-f", ".gitmodules", "--rename-section", "submodule.vendor/lib", "submodule.../../x")
	git(t, parent, "commit", "-q", "-am", "evil name")
	on := true
	res := run(t, parent, Options{Submodules: &on}, NewBlocks())
	if s := res.Record.Submodules[0]; s.Record != nil || s.Name != "../../x" || !strings.Contains(s.Note, "unsafe") {
		t.Fatalf("unsafe name: %+v", s)
	}
	for _, n := range []string{"a", "a/b", "vendor/lib"} {
		if !safeSubmoduleName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", "..", "a/../b", "/abs", "a//b", "a\\b", "./a"} {
		if safeSubmoduleName(n) {
			t.Errorf("%q allowed", n)
		}
	}
}
