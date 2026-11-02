package archive

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
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
