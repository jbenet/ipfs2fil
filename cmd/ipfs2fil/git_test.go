package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// ipfs2fil takes a plain git working tree as --repo.
func TestPackGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	t.Setenv("HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "project")
	os.Mkdir(src, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "one"}} {
		if args[0] == "add" {
			os.WriteFile(filepath.Join(src, "a.txt"), []byte("a\n"), 0o644)
		}
		c := exec.Command("git", args...)
		c.Dir = src
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out, _, err := ipfs2fil(t, "scan", "--repo", src)
	if err != nil || !strings.Contains(out, "git repo") {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if _, _, err := ipfs2fil(t, "pack", "--repo", src, "--out", dir); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Read(filepath.Join(dir, "manifest.json"))
	if err != nil || m.Source.Kind != "git" || len(m.Roots) != 1 || m.Roots[0].Name != "project" {
		t.Fatalf("manifest %+v %v", m, err)
	}
}
