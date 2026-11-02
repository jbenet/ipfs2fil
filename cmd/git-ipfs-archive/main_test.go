package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func cli(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := run(context.Background(), args, &out, &errb)
	return out.String() + errb.String(), err
}

func TestArchiveSinceRestoreVerifyShow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, cars := t.TempDir(), t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a\n"), 0o644)
	git(t, repo, "add", "a")
	git(t, repo, "commit", "-q", "-m", "one")

	if out, err := cli(t, repo, "-o", filepath.Join(cars, "1.car"), "--set", "ipfs2fil.copies=3"); err != nil {
		t.Fatal(out, err)
	}
	if out, _ := cli(t, repo, "--since", cars, "-o", filepath.Join(cars, "2.car")); !strings.Contains(out, "nothing changed") {
		t.Fatalf("unchanged run: %s", out)
	}
	os.WriteFile(filepath.Join(repo, "b"), []byte("b\n"), 0o644)
	git(t, repo, "add", "b")
	git(t, repo, "commit", "-q", "-m", "two")
	if out, err := cli(t, repo, "--since", cars, "-o", filepath.Join(cars, "2.car")); err != nil || !strings.Contains(out, "delta") {
		t.Fatal(out, err)
	}
	dest := filepath.Join(t.TempDir(), "r.git")
	if out, err := cli(t, "restore", cars, dest); err != nil {
		t.Fatal(out, err)
	}
	if a, b := git(t, repo, "rev-parse", "main"), git(t, dest, "rev-parse", "main"); a != b {
		t.Fatalf("main %s vs %s", a, b)
	}
	if out, err := cli(t, "restore", cars, dest); err == nil {
		t.Fatalf("restore into a non-empty dir: %s", out)
	}
	if out, err := cli(t, "verify", cars); err != nil || !strings.HasPrefix(out, "ok ") {
		t.Fatal(out, err)
	}
	out, err := cli(t, "show", cars)
	if err != nil {
		t.Fatal(err)
	}
	var snaps []struct {
		Record struct {
			Instructions map[string]string
			Commits      int
		}
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil || len(snaps) != 2 {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if snaps[0].Record.Instructions["ipfs2fil.copies"] != "3" || snaps[1].Record.Commits != 1 {
		t.Fatalf("show: %+v", snaps)
	}
	// A CAR that was cut short is refused, not half restored.
	whole, _ := os.ReadFile(filepath.Join(cars, "1.car"))
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, "1.car"), whole[:len(whole)-10], 0o644)
	if out, err := cli(t, "verify", bad); err == nil {
		t.Fatalf("truncated CAR verified: %s", out)
	}
}

func TestCompactedCARStandsAlone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo, cars := t.TempDir(), t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	var roots []string
	for i, f := range []string{"a", "b", "c"} {
		os.WriteFile(filepath.Join(repo, f), []byte(f+"\n"), 0o644)
		git(t, repo, "add", f)
		git(t, repo, "commit", "-q", "-m", f)
		args := []string{repo, "-o", filepath.Join(cars, f+".car"), "--compact", "never"}
		if i > 0 {
			args = append(args, "--since", cars)
		}
		out, err := cli(t, args...)
		if err != nil {
			t.Fatal(out, err)
		}
		roots = append(roots, strings.Fields(out)[0])
	}
	git(t, repo, "branch", "old")
	alone := t.TempDir()
	out, err := cli(t, repo, "--since", cars, "--compact", "now", "-o", filepath.Join(alone, "d.car"))
	if err != nil || !strings.Contains(out, "compacted") || !strings.Contains(out, "restores all 3 earlier snapshots") {
		t.Fatal(out, err)
	}
	// The compacted CAR alone restores the newest state and each earlier one.
	if out, err := cli(t, "verify", alone); err != nil || !strings.Contains(out, "1 snapshots, 1 packs, 2 refs") {
		t.Fatal(out, err)
	}
	for i, r := range roots {
		dest := filepath.Join(t.TempDir(), "r.git")
		out, err := cli(t, "restore", "--at", r, alone, dest)
		if err != nil || !strings.Contains(out, "from the compacted snapshot's history") {
			t.Fatal(out, err)
		}
		if got := git(t, dest, "rev-list", "--count", "main"); got != fmt.Sprint(i+1) {
			t.Fatalf("snapshot %d: main has %s commits", i, got)
		}
		if out, err := cli(t, "verify", "--at", r, alone); err != nil || !strings.HasPrefix(out, "ok "+r) {
			t.Fatal(out, err)
		}
	}
	// Beside the earlier CARs, the compacted one is the newest.
	os.Rename(filepath.Join(alone, "d.car"), filepath.Join(cars, "d.car"))
	if out, err := cli(t, "verify", cars); err != nil || !strings.Contains(out, "1 snapshots") {
		t.Fatal(out, err)
	}
	out, err = cli(t, "show", cars)
	var snaps []struct {
		Root      string
		Compacted *struct{ Refs []any }
	}
	if err != nil || json.Unmarshal([]byte(out), &snaps) != nil || len(snaps) != 4 || snaps[0].Root != roots[0] || snaps[0].Compacted == nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
}
