package archive

import (
	"context"
	"crypto/rand"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=A", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=A", "GIT_COMMITTER_EMAIL=a@x",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// After a push, a run over a git repo with one new commit keeps the stored
// piece with its history and adds only the delta's small blocks.
func TestGitRepoRerunKeepsPieces(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := repo.GitDir
	repo.GitDir = t.TempDir()
	defer func() { repo.GitDir = old }()
	src := filepath.Join(t.TempDir(), "project")
	os.Mkdir(src, 0o755)
	gitCmd(t, src, "init", "-q", "-b", "main")
	for i := range 4 {
		b := make([]byte, 3<<20) // incompressible, so the pack spans pieces
		rand.Read(b)
		name := filepath.Join(src, string(rune('a'+i)))
		os.WriteFile(name, b, 0o644)
		gitCmd(t, src, "add", ".")
		gitCmd(t, src, "commit", "-q", "-m", "c")
	}
	dir := t.TempDir()
	opts := testOptions()
	opts.MinPiece, opts.MaxPiece = 32<<20, 64<<20 // one data piece, as a repo under 512 MiB is
	build := func(t *testing.T, r repo.Repo, dir string) *Result {
		res, err := Build(t.Context(), r, dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	open := func() repo.Repo {
		r, err := repo.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := build(t, open(), dir)
	check(t, a)
	data := func(res *Result) map[string]int64 {
		m := map[string]int64{}
		for _, p := range res.Manifest.Pieces {
			if p.Part != manifest.PartIndex && p.Part != manifest.PartMeta && p.Part != manifest.PartHead {
				m[p.PieceCID] = p.CARBytes
			}
		}
		return m
	}
	before := data(a)
	stored(t, dir) // pushed

	os.WriteFile(filepath.Join(src, "e"), []byte("one more\n"), 0o644)
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-q", "-m", "small")
	b := build(t, open(), dir)
	check(t, b)
	var kept, fresh int64
	for pc, n := range data(b) {
		if _, ok := before[pc]; ok {
			kept += n
		} else {
			fresh += n
		}
	}
	t.Logf("data pieces: %d bytes kept, %d new", kept, fresh)
	if kept < 8<<20 || fresh*10 > kept {
		t.Errorf("data pieces: %d bytes kept, %d new", kept, fresh)
	}
}

// A compacted git snapshot stands alone: the next build keeps none of the
// data pieces the earlier snapshots used, and notes them as dropped, so a
// push removes them.
func TestGitCompactionDropsOldPieces(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := repo.GitDir
	repo.GitDir = t.TempDir()
	defer func() { repo.GitDir, repo.GitCompact = old, "" }()
	src := filepath.Join(t.TempDir(), "project")
	os.Mkdir(src, 0o755)
	gitCmd(t, src, "init", "-q", "-b", "main")
	for i := range 4 {
		b := make([]byte, 3<<20)
		rand.Read(b)
		os.WriteFile(filepath.Join(src, string(rune('a'+i))), b, 0o644)
		gitCmd(t, src, "add", ".")
		gitCmd(t, src, "commit", "-q", "-m", "c")
	}
	dir := t.TempDir()
	opts := testOptions()
	opts.MinPiece, opts.MaxPiece = 32<<20, 64<<20
	build := func() *Result {
		r, err := repo.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Build(t.Context(), r, dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		check(t, res)
		return res
	}
	a := build()
	stored(t, dir)
	os.WriteFile(filepath.Join(src, "e"), []byte("one more\n"), 0o644)
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-q", "-m", "small")
	b := build() // a delta: keeps the data piece
	stored(t, dir)
	var dataA []string
	for _, p := range a.Manifest.Pieces {
		if p.Part != manifest.PartIndex && p.Part != manifest.PartMeta && p.Part != manifest.PartHead {
			dataA = append(dataA, p.PieceCID)
		}
	}
	inB := map[string]bool{}
	for _, p := range b.Manifest.Pieces {
		inB[p.PieceCID] = true
	}
	for _, pc := range dataA {
		if !inB[pc] {
			t.Fatalf("the delta did not keep data piece %s", pc)
		}
	}

	repo.GitCompact = "now"
	os.WriteFile(filepath.Join(src, "f"), []byte("and another\n"), 0o644)
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-q", "-m", "small")
	c := build()
	inC := map[string]bool{}
	for _, p := range c.Manifest.Pieces {
		inC[p.PieceCID] = true
	}
	for _, pc := range dataA {
		if inC[pc] || !slices.Contains(c.Manifest.Dropped, pc) {
			t.Errorf("data piece %s: still used %v, dropped %v", pc, inC[pc], slices.Contains(c.Manifest.Dropped, pc))
		}
	}
	if len(c.Missing) > 0 {
		t.Errorf("compacted archive misses %d blocks", len(c.Missing))
	}
}

// A git repo whose config asks for tip folders lists them in Files.
func TestGitTipFolderEntries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	old := repo.GitDir
	repo.GitDir = t.TempDir()
	defer func() { repo.GitDir = old }()
	src := filepath.Join(t.TempDir(), "project")
	os.MkdirAll(filepath.Join(src, "docs"), 0o755)
	gitCmd(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "docs", "a.md"), []byte("# a\n"), 0o644)
	os.WriteFile(filepath.Join(src, ".ipfs-archive-config"), []byte("[archive]\n\ttips = default\n"), 0o644)
	gitCmd(t, src, "add", ".")
	gitCmd(t, src, "commit", "-q", "-m", "c")
	r, err := repo.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l := newLayout(r)
	roots, err := l.walkRoots(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.cut(testOptions()); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{}
	owned := make([]bool, len(l.tab))
	for _, rt := range roots {
		m.Roots = append(m.Roots, l.rootEntry(rt, owned))
	}
	l.gits = l.gitSummaries(ctx, m.Roots)
	got := map[string]string{}
	for _, e := range l.entries(roots, m) {
		got[strings.TrimPrefix(e.Path, m.Roots[0].Path)] = e.Name + " " + e.Kind
	}
	want := map[string]string{"": "project folder", "/main": "main folder", "/main/.ipfs-archive-config": ".ipfs-archive-config text",
		"/main/docs": "docs folder", "/main/docs/a.md": "a.md text"}
	if !maps.Equal(got, want) {
		t.Fatalf("entries %q", got)
	}
}
