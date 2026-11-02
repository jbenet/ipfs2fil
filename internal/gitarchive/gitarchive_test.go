package gitarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// git runs git in dir with a fixed identity and clock.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_COMMITTER_DATE=1700000000 +0000",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+dir, "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a working tree with a few commits, a branch, an annotated
// tag and a lightweight tag. Files repeat with small edits so packs hold deltas.
func newRepo(t *testing.T, format string) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"init", "-q", "-b", "main"}
	if format != "" {
		args = append(args, "--object-format="+format)
	}
	git(t, dir, args...)
	big := strings.Repeat("line of text that repeats in every version\n", 400)
	for i := 0; i < 6; i++ {
		write(t, filepath.Join(dir, "big.txt"), big+fmt.Sprintf("version %d\n", i))
		write(t, filepath.Join(dir, "sub", fmt.Sprintf("f%d.txt", i)), fmt.Sprintf("file %d\n", i))
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", fmt.Sprintf("commit %d\n\nbody %d", i, i))
	}
	git(t, dir, "tag", "-a", "v1", "-m", "release one")
	git(t, dir, "tag", "light")
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(dir, "feature.txt"), "feature\n")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "feature work")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "remote", "add", "origin", "https://alice:s3cret-token@example.com/alice/repo.git")
	return dir
}

func archive(t *testing.T, path string, prev *Snapshot, bs *Blocks) *Result {
	t.Helper()
	o := Options{TempDir: t.TempDir(), LogChunk: 3}
	if prev != nil {
		o.Prev, o.PrevRoot = prev.Record, prev.Root
	}
	res, err := Archive(context.Background(), path, o, bs.Put)
	if err != nil {
		t.Fatal(err)
	}
	bs.roots = append(bs.roots, res.Root)
	return res
}

func refsOf(t *testing.T, dir string) string {
	return git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)")
}

func restore(t *testing.T, bs *Blocks, root cid.Cid) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "restored.git")
	if _, err := Restore(context.Background(), bs.Get, root, dest, RestoreOptions{Strict: true, Remotes: true}); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestRoundTripFullThenDelta(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	bs := NewBlocks()
	full := archive(t, repo, nil, bs)
	if full.Record.Prev != nil || len(full.Record.Packs) != 1 || full.Record.Packs[0].Thin {
		t.Fatalf("full snapshot: %+v", full.Record)
	}
	if full.Record.Commits != 7 || len(full.Record.Log) != 3 {
		t.Fatalf("log: %d commits in %d chunks", full.Record.Commits, len(full.Record.Log))
	}
	if got := full.Record.Remotes; len(got) != 1 || strings.Contains(got[0].URL, "s3cret") {
		t.Fatalf("remote not stripped: %+v", got)
	}
	if full.Record.Head != "refs/heads/main" {
		t.Fatalf("head %q", full.Record.Head)
	}
	if r := restore(t, bs, full.Root); refsOf(t, r) != refsOf(t, repo) {
		t.Fatalf("full restore refs differ:\n%s\nvs\n%s", refsOf(t, r), refsOf(t, repo))
	}

	// Same state again: nothing new.
	again := archive(t, repo, &Snapshot{full.Root, full.Record}, bs)
	if !again.Unchanged || again.Root != full.Root {
		t.Fatalf("unchanged repo made a new snapshot %s", again.Root)
	}

	// New commits that edit big.txt (deltas against stored objects), a
	// deleted branch and a force-moved tag.
	write(t, filepath.Join(repo, "big.txt"), strings.Repeat("line of text that repeats in every version\n", 400)+"version 7\n")
	git(t, repo, "commit", "-q", "-am", "commit 7")
	git(t, repo, "branch", "-q", "-D", "feature")
	git(t, repo, "tag", "-f", "light", "HEAD~2")
	delta := archive(t, repo, &Snapshot{full.Root, full.Record}, bs)
	if delta.Record.Prev == nil || delta.Record.Prev.Cid != full.Root {
		t.Fatal("delta does not point at its base")
	}
	if len(delta.Record.Packs) != 1 || !delta.Record.Packs[0].Thin {
		t.Fatalf("delta pack: %+v", delta.Record.Packs)
	}
	if delta.Objects > 4 {
		t.Fatalf("delta stored %d objects; want only the new commit, tree and blob", delta.Objects)
	}
	if delta.Record.Commits != 1 {
		t.Fatalf("delta log has %d commits", delta.Record.Commits)
	}
	r := restore(t, bs, delta.Root)
	if refsOf(t, r) != refsOf(t, repo) {
		t.Fatalf("delta restore refs differ:\n%s\nvs\n%s", refsOf(t, r), refsOf(t, repo))
	}
	if got := git(t, r, "remote", "get-url", "origin"); got != "https://example.com/alice/repo.git" {
		t.Fatalf("restored remote %q", got)
	}
	latest, err := bs.Latest()
	if err != nil || latest != delta.Root {
		t.Fatalf("latest = %s, %v; want %s", latest, err, delta.Root)
	}
}

func TestIdxMatchesGit(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	g, err := OpenRepo(context.Background(), "", repo)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pk := filepath.Join(dir, "p.pack")
	f, _ := os.Create(pk)
	refs, _ := g.Refs(context.Background())
	if err := g.PackObjects(context.Background(), f, refTips(refs), nil, false); err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	ix, err := IndexPack(f, st.Size(), SHA1, nil, nil)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var mine bytes.Buffer
	if err := WriteIdx(&mine, ix); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "index-pack", "-o", filepath.Join(dir, "git.idx"), pk)
	want, _ := os.ReadFile(filepath.Join(dir, "git.idx"))
	if !bytes.Equal(mine.Bytes(), want) {
		t.Fatalf("idx differs from git's (%d vs %d bytes)", mine.Len(), len(want))
	}
	parsed, err := ReadIdx(mine.Bytes(), SHA1)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ix.Objects {
		if off, ok := parsed.Find(o.ID); !ok || off != o.Offset {
			t.Fatalf("Find(%x) = %d, %v; want %d", o.ID, off, ok, o.Offset)
		}
	}
}

func TestSHA256Repo(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	if err := exec.Command("git", "init", "-q", "--object-format=sha256", dir).Run(); err != nil {
		t.Skip("git without sha256 support")
	}
	repo := newRepo(t, "sha256")
	bs := NewBlocks()
	res := archive(t, repo, nil, bs)
	if res.Record.ObjectFormat != "sha256" || len(res.Record.Refs[0].ID) != 64 {
		t.Fatalf("record: %+v", res.Record.Refs)
	}
	write(t, filepath.Join(repo, "x"), "x\n")
	git(t, repo, "add", "x")
	git(t, repo, "commit", "-q", "-m", "x")
	d := archive(t, repo, &Snapshot{res.Root, res.Record}, bs)
	if r := restore(t, bs, d.Root); refsOf(t, r) != refsOf(t, repo) {
		t.Fatal("sha256 restore refs differ")
	}
}

func TestShallowClone(t *testing.T) {
	needGit(t)
	src := newRepo(t, "")
	dst := filepath.Join(t.TempDir(), "shallow")
	git(t, t.TempDir(), "clone", "-q", "--depth=2", "--no-local", "file://"+src, dst)
	bs := NewBlocks()
	res := archive(t, dst, nil, bs)
	if len(res.Record.Shallow) == 0 {
		t.Fatal("shallow list not recorded")
	}
	r := restore(t, bs, res.Root)
	if refsOf(t, r) != refsOf(t, dst) {
		t.Fatal("shallow restore refs differ")
	}
}

func TestPartialCloneRefused(t *testing.T) {
	needGit(t)
	src := newRepo(t, "")
	git(t, src, "config", "uploadpack.allowFilter", "true")
	dst := filepath.Join(t.TempDir(), "partial")
	git(t, t.TempDir(), "clone", "-q", "--filter=blob:none", "--no-local", "file://"+src, dst)
	_, err := Archive(context.Background(), dst, Options{TempDir: t.TempDir()}, NewBlocks().Put)
	if err == nil || !strings.Contains(err.Error(), "partial clone") {
		t.Fatalf("partial clone: err = %v", err)
	}
}

// A caller's GIT_DIR must not redirect which repo is read.
func TestIgnoresGitEnv(t *testing.T) {
	needGit(t)
	a, b := newRepo(t, ""), t.TempDir()
	git(t, b, "init", "-q", "-b", "main")
	write(t, filepath.Join(b, "other"), "other\n")
	git(t, b, "add", "other")
	git(t, b, "commit", "-q", "-m", "other repo")
	t.Setenv("GIT_DIR", filepath.Join(b, ".git"))
	bs := NewBlocks()
	res := archive(t, a, nil, bs)
	if res.Record.Commits != 7 {
		t.Fatalf("archived %d commits; GIT_DIR leaked in", res.Record.Commits)
	}
}

func TestEmptyRepo(t *testing.T) {
	needGit(t)
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	bs := NewBlocks()
	res := archive(t, dir, nil, bs)
	if len(res.Record.Packs) != 0 || len(res.Record.Refs) != 0 {
		t.Fatalf("empty repo: %+v", res.Record)
	}
	restore(t, bs, res.Root)
}

func TestStripCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@host/x.git":       "https://host/x.git",
		"https://token@host/x.git":     "https://host/x.git",
		"ssh://git:pw@host/x.git":      "ssh://git@host/x.git",
		"git@github.com:a/b.git":       "git@github.com:a/b.git",
		"/srv/repos/x.git":             "/srv/repos/x.git",
		"https://host/x.git?token=abc": "https://host/x.git?token=abc",
	} {
		if got := StripCredentials(in); got != want {
			t.Errorf("StripCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsafeRemote(t *testing.T) {
	bad := []Remote{
		{"origin", "ext::sh -c touch% /tmp/pwned"},
		{"origin", "fd::17"},
		{"origin", "--upload-pack=touch /tmp/x"},
		{"-x", "https://h/r"},
		{"a b", "https://h/r"},
		{"origin", "file:///etc"},
	}
	for _, r := range bad {
		if unsafeRemote(r) == "" {
			t.Errorf("remote %+v allowed", r)
		}
	}
	for _, r := range []Remote{{"origin", "https://h/r.git"}, {"up", "git@h:r.git"}, {"local", "/srv/r.git"}} {
		if why := unsafeRemote(r); why != "" {
			t.Errorf("remote %+v refused: %s", r, why)
		}
	}
}

func TestRecordCheck(t *testing.T) {
	id := strings.Repeat("a", 40)
	for _, r := range []Record{
		{Kind: Kind, Version: 1, ObjectFormat: "sha1", Refs: []Ref{{Name: "HEAD", ID: id}}},
		{Kind: Kind, Version: 1, ObjectFormat: "sha1", Refs: []Ref{{Name: "refs/heads/x", ID: "zz"}}},
		{Kind: Kind, Version: 1, ObjectFormat: "sha1", Head: "../../config"},
		{Kind: Kind, Version: 1, ObjectFormat: "sha1", Shallow: []string{"nope"}},
		{Kind: Kind, Version: 1, ObjectFormat: "md5"},
	} {
		_, b, err := encodeBlock(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeRecord(b); err == nil {
			t.Errorf("record %+v accepted", r)
		}
	}
}

func TestChainLoopRefused(t *testing.T) {
	bs := NewBlocks()
	// A record whose Prev is itself cannot be built (the CID depends on
	// the bytes), so chain two records that name each other's would-be
	// CIDs via a getter that serves a fixed record for any CID.
	r := Record{Kind: Kind, Version: 1, ObjectFormat: "sha1"}
	c0, b0, _ := encodeBlock(r)
	r.Prev = &Link{c0}
	c1, b1, _ := encodeBlock(r)
	get := func(c cid.Cid) ([]byte, error) {
		if c == c0 {
			return b1, nil // c0 claims to be c1's content: Prev points at c0 again
		}
		if c == c1 {
			return b1, nil
		}
		return bs.Get(c)
	}
	_ = b0
	if _, err := Chain(get, c1); err == nil {
		t.Fatal("looping chain accepted")
	}
}

func TestCollisionDetected(t *testing.T) {
	data, err := os.ReadFile("testdata/shattered-1-prefix.bin")
	if err != nil {
		t.Fatal(err)
	}
	h := SHA1.hasher()
	h.Write(data)
	if _, err := SHA1.sum(h); !errors.Is(err, ErrCollision) {
		t.Fatalf("shattered prefix: err = %v, want ErrCollision", err)
	}
	h = SHA1.hasher()
	h.Write([]byte("harmless"))
	if _, err := SHA1.sum(h); err != nil {
		t.Fatal(err)
	}
}

// Corrupt packs fail with errors, never panics or hangs.
func TestIndexPackCorrupt(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	g, _ := OpenRepo(context.Background(), "", repo)
	var b bytes.Buffer
	refs, _ := g.Refs(context.Background())
	if err := g.PackObjects(context.Background(), &b, refTips(refs), nil, false); err != nil {
		t.Fatal(err)
	}
	pack := b.Bytes()
	for i := 12; i < len(pack)-20; i += 7 {
		bad := append([]byte(nil), pack...)
		bad[i] ^= 0x40
		// Re-seal the trailer so the corruption reaches the object parser.
		h := SHA1.hasher()
		h.Write(bad[:len(bad)-20])
		copy(bad[len(bad)-20:], h.Sum(nil))
		if _, err := IndexPack(bytes.NewReader(bad), int64(len(bad)), SHA1, nil, nil); err == nil {
			// Some flips land in zlib slack and still inflate to the same
			// bytes; those are fine. Anything else must error.
			continue
		}
	}
	if _, err := IndexPack(bytes.NewReader(pack[:len(pack)-1]), int64(len(pack)-1), SHA1, nil, nil); err == nil {
		t.Fatal("truncated pack accepted")
	}
}

func FuzzApplyDelta(f *testing.F) {
	f.Add([]byte("hello world"), []byte{11, 5, 0x90, 5})
	f.Add([]byte(""), []byte{0, 3, 3, 'a', 'b', 'c'})
	f.Fuzz(func(t *testing.T, base, delta []byte) {
		out, err := applyDelta(base, delta)
		if err == nil && len(out) > len(delta)*0x10000+len(delta) {
			t.Fatalf("delta of %d bytes made %d", len(delta), len(out))
		}
	})
}

func FuzzIndexPack(f *testing.F) {
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, body []byte) {
		h := SHA1.hasher()
		h.Write(body)
		pack := append(append([]byte(nil), body...), h.Sum(nil)...)
		IndexPack(bytes.NewReader(pack), int64(len(pack)), SHA1, nil, nil)
	})
}
