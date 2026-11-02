package gitarchive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
)

// step archives repo on top of prev (nil: a full archive), with every block
// in all, and returns what this run alone put, so a test can check what a
// snapshot needs.
func step(t *testing.T, repo string, o Options, prev *Snapshot, all *Blocks) (*Result, *Blocks) {
	t.Helper()
	o.TempDir = t.TempDir()
	o.LogChunk = 3
	if prev != nil {
		o.Prev, o.PrevRoot, o.Get, o.Have = prev.Record, prev.Root, all.Get, all.Has
	}
	mine := NewBlocks()
	res, err := Archive(context.Background(), repo, o, func(c cid.Cid, b []byte) error {
		if err := mine.Put(c, b); err != nil {
			return err
		}
		return all.Put(c, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged {
		all.roots = append(all.roots, res.Root)
		mine.roots = append(mine.roots, res.Root)
	}
	return res, mine
}

func commit(t *testing.T, repo string, i int) {
	t.Helper()
	write(t, filepath.Join(repo, "big.txt"), strings.Repeat("line of text that repeats in every version\n", 400)+fmt.Sprintf("version %d\n", 100+i))
	write(t, filepath.Join(repo, fmt.Sprintf("n%d.txt", i)), fmt.Sprintf("new %d\n", i))
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", fmt.Sprintf("later %d", i))
}

// merged is a block set of several runs' blocks.
func merged(sets ...*Blocks) *Blocks {
	out := NewBlocks()
	for _, s := range sets {
		for k, v := range s.m {
			out.m[k] = v
		}
		out.roots = append(out.roots, s.roots...)
	}
	return out
}

func restoreAt(t *testing.T, bs *Blocks, root, at cid.Cid) (string, *RestoreResult) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "r.git")
	rr, err := Restore(context.Background(), bs.Get, root, dest, RestoreOptions{Strict: true, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return dest, rr
}

func refLines(refs []Ref) string {
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "%s %s\n", r.ID, r.Name)
	}
	return strings.TrimSpace(b.String())
}

// TestCompactStandsAlone: after deltas, a compacted snapshot restores the
// repo from its own blocks, and every earlier snapshot too, even one whose
// branch the repo has since deleted and gc'd.
func TestCompactStandsAlone(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	all := NewBlocks()
	full, _ := step(t, repo, Options{}, nil, all)
	snaps := []Snapshot{{full.Root, full.Record}}

	// A branch that a later snapshot no longer has, gone from the repo.
	git(t, repo, "checkout", "-q", "-b", "doomed")
	write(t, filepath.Join(repo, "doomed.txt"), "only here\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "doomed work")
	git(t, repo, "checkout", "-q", "main")
	d1, _ := step(t, repo, Options{}, &snaps[0], all)
	snaps = append(snaps, Snapshot{d1.Root, d1.Record})
	doomed := git(t, repo, "rev-parse", "doomed")

	git(t, repo, "branch", "-q", "-D", "doomed")
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "-q", "--prune=now")
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	commit(t, repo, 1)
	d2, _ := step(t, repo, Options{}, &snaps[1], all)
	snaps = append(snaps, Snapshot{d2.Root, d2.Record})
	if d2.Compacted != "" || d2.Record.Base {
		t.Fatalf("delta compacted: %q", d2.Compacted)
	}

	commit(t, repo, 2)
	c, mine := step(t, repo, Options{Compact: CompactNow}, &snaps[2], all)
	r := c.Record
	if c.Compacted == "" || !r.Base || r.Prev != nil || len(r.Packs) != 1 || r.Packs[0].Thin {
		t.Fatalf("compacted: %q base %v prev %v packs %+v", c.Compacted, r.Base, r.Prev, r.Packs)
	}
	if len(r.History) != 3 {
		t.Fatalf("history has %d entries, want 3", len(r.History))
	}
	if r.Commits != 10 {
		t.Fatalf("compacted log has %d commits, want all 10", r.Commits)
	}
	found := false
	for _, n := range c.Notes {
		found = found || strings.Contains(n, "no longer has")
	}
	if !found {
		t.Fatalf("doomed branch was gc'd, so objects should come from the archive; notes %v", c.Notes)
	}

	// Only the compacted run's blocks: the newest state restores...
	dest, rr := restoreAt(t, mine, c.Root, cid.Undef)
	if refsOf(t, dest) != refsOf(t, repo) || rr.Snapshots != 1 || rr.Packs != 1 {
		t.Fatalf("restore from the compacted snapshot: %+v\n%s\nvs\n%s", rr, refsOf(t, dest), refsOf(t, repo))
	}
	// ...and so does every earlier one, from its history.
	for i, s := range snaps {
		dest, rr := restoreAt(t, mine, c.Root, s.Root)
		if !rr.FromHistory {
			t.Fatalf("snapshot %d: not restored from history", i)
		}
		if got, want := refsOf(t, dest), refLines(s.Record.Refs); got != want {
			t.Fatalf("snapshot %d refs:\n%s\nwant\n%s", i, got, want)
		}
	}
	dest, _ = restoreAt(t, mine, c.Root, d1.Root)
	if got := git(t, dest, "rev-parse", "refs/heads/doomed"); got != doomed {
		t.Fatalf("doomed = %s, want %s", got, doomed)
	}

	// With every block, an earlier snapshot still names its own blocks,
	// but the history is preferred, since a compacted archive's old
	// records can outlive their packs.
	if _, rr := restoreAt(t, all, c.Root, d1.Root); !rr.FromHistory {
		t.Fatal("history not preferred")
	}

	// A delta on top builds on the compacted snapshot alone.
	commit(t, repo, 3)
	d3, more := step(t, repo, Options{}, &Snapshot{c.Root, c.Record}, all)
	if d3.Record.Base || d3.Record.Prev == nil || d3.Record.Prev.Cid != c.Root || !d3.Record.Packs[0].Thin {
		t.Fatalf("delta after compaction: %+v", d3.Record)
	}
	both := merged(mine, more)
	if dest, rr := restoreAt(t, both, d3.Root, cid.Undef); refsOf(t, dest) != refsOf(t, repo) || rr.Snapshots != 2 {
		t.Fatalf("restore after compaction: %+v", rr)
	}
	if dest, rr := restoreAt(t, both, d3.Root, snaps[0].Root); !rr.FromHistory || refsOf(t, dest) != refLines(full.Record.Refs) {
		t.Fatalf("first snapshot via the later delta: %+v", rr)
	}
	if dest, _ := restoreAt(t, both, d3.Root, c.Root); refsOf(t, dest) != refLines(c.Record.Refs) {
		t.Fatal("compacted snapshot itself via the later delta")
	}

	// The newest root wins among all of them; the compacted one
	// supersedes the snapshots it lists.
	if l, err := all.Latest(); err != nil || l != d3.Root {
		t.Fatalf("latest %s %v, want %s", l, err, d3.Root)
	}
	if l, err := both.Latest(); err != nil || l != d3.Root {
		t.Fatalf("latest of the compacted and later blocks %s %v", l, err)
	}
	// Even with the earlier snapshots' CARs left beside it, and nothing after.
	old := merged(all)
	old.roots = []cid.Cid{full.Root, d1.Root, d2.Root, c.Root}
	if l, err := old.Latest(); err != nil || l != c.Root {
		t.Fatalf("latest %s %v, want the compacted %s", l, err, c.Root)
	}

	// The summary lists every commit once, and every snapshot.
	s, err := Summarize(both.Get, d3.Root, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Commits) != 11 || len(s.Snapshots) != 5 || s.Missing != 0 {
		t.Fatalf("summary: %d commits, %d snapshots, %d missing", len(s.Commits), len(s.Snapshots), s.Missing)
	}
	if !s.Snapshots[1].Base || !s.Snapshots[2].Compacted || s.Snapshots[4].Root != full.Root.String() {
		t.Fatalf("snapshots %+v", s.Snapshots)
	}

	// A snapshot nobody knows is an error, not a silent full restore.
	if _, err := Restore(context.Background(), both.Get, d3.Root, filepath.Join(t.TempDir(), "x"), RestoreOptions{At: d3.Record.Packs[0].Pack.Cid}); err == nil {
		t.Fatal("restored an unknown snapshot")
	}
}

// TestCompactAuto: compaction happens by itself once thin packs pile up,
// and a second compaction carries the first one's history.
func TestCompactAuto(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	all := NewBlocks()
	res, _ := step(t, repo, Options{}, nil, all)
	prev := Snapshot{res.Root, res.Record}
	var compacted []int
	var hist []int
	for i := 1; i <= 7; i++ {
		commit(t, repo, i)
		// tiny deltas: only the count triggers
		res, _ = step(t, repo, Options{CompactAfter: 2}, &prev, all)
		if res.Compacted != "" {
			compacted = append(compacted, i)
			hist = append(hist, len(res.Record.History))
		}
		prev = Snapshot{res.Root, res.Record}
	}
	// two thin packs since the last full, then the next run compacts
	if fmt.Sprint(compacted) != "[3 6]" || fmt.Sprint(hist) != "[3 6]" {
		t.Fatalf("compacted at %v with history %v", compacted, hist)
	}

	// never means never; an unchanged repo is not compacted unless asked
	for i := 8; i <= 12; i++ {
		commit(t, repo, i)
		res, _ = step(t, repo, Options{CompactAfter: 1, Compact: CompactNever}, &prev, all)
		if res.Compacted != "" {
			t.Fatal("compacted with never")
		}
		prev = Snapshot{res.Root, res.Record}
	}
	same, _ := step(t, repo, Options{CompactAfter: 1}, &prev, all)
	if !same.Unchanged {
		t.Fatal("unchanged repo compacted on auto")
	}
	now, mine := step(t, repo, Options{Compact: CompactNow}, &prev, all)
	if now.Unchanged || now.Compacted == "" || len(now.Record.History) != 13 {
		t.Fatalf("compact now: unchanged %v, %q, history %d", now.Unchanged, now.Compacted, len(now.Record.History))
	}
	if dest, _ := restoreAt(t, mine, now.Root, prev.Root); refsOf(t, dest) != refsOf(t, repo) {
		t.Fatal("restore of the last snapshot from history")
	}

	// share: one delta bigger than half the full pack compacts
	big := t.TempDir()
	git(t, big, "init", "-q", "-b", "main")
	write(t, filepath.Join(big, "a"), "small\n")
	git(t, big, "add", "-A")
	git(t, big, "commit", "-q", "-m", "one")
	all = NewBlocks()
	b0, _ := step(t, big, Options{}, nil, all)
	write(t, filepath.Join(big, "b"), strings.Repeat("unrelated bytes that do not delta against the first file\n", 50))
	git(t, big, "add", "-A")
	git(t, big, "commit", "-q", "-m", "two")
	b1, _ := step(t, big, Options{}, &Snapshot{b0.Root, b0.Record}, all)
	if b1.Compacted != "" {
		t.Fatal("the first big delta has nothing to compact yet")
	}
	git(t, big, "commit", "-q", "--allow-empty", "-m", "three")
	b2, _ := step(t, big, Options{}, &Snapshot{b1.Root, b1.Record}, all)
	if b2.Compacted == "" || !strings.Contains(b2.Compacted, "half") {
		t.Fatalf("not compacted by size: %q (full %d, thin %d)", b2.Compacted, b0.PackBytes, b1.PackBytes)
	}
}

// TestCompactLFS: a compacted snapshot lists and holds every LFS object,
// even ones the repo no longer has.
func TestCompactLFS(t *testing.T) {
	needGit(t)
	dir := newRepo(t, "")
	gd := filepath.Join(dir, ".git")
	a := putLFS(t, gd, strings.Repeat("dataset ", 300000), "")
	all := NewBlocks()
	r0, _ := step(t, dir, Options{}, nil, all)
	os.RemoveAll(filepath.Join(gd, "lfs"))
	b := putLFS(t, gd, "later object", "")
	commit(t, dir, 1)
	r1, _ := step(t, dir, Options{}, &Snapshot{r0.Root, r0.Record}, all)
	c, mine := step(t, dir, Options{Compact: CompactNow}, &Snapshot{r1.Root, r1.Record}, all)
	if c.Record.LFSObjects != 2 {
		t.Fatalf("compacted LFS list has %d objects", c.Record.LFSObjects)
	}
	dest := filepath.Join(t.TempDir(), "r.git")
	rr, err := Restore(context.Background(), mine.Get, c.Root, dest, RestoreOptions{})
	if err != nil || rr.LFS != 2 {
		t.Fatalf("restore %+v %v", rr, err)
	}
	for _, oid := range []string{a, b} {
		if _, err := os.Stat(filepath.Join(dest, "lfs", "objects", oid[:2], oid[2:4], oid)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCompactSubmodules: a compacted snapshot's submodule archives stand
// alone too.
func TestCompactSubmodules(t *testing.T) {
	needGit(t)
	parent, _ := subRepos(t)
	on := true
	all := NewBlocks()
	r0, _ := step(t, parent, Options{Submodules: &on}, nil, all)
	commit(t, parent, 1)
	r1, _ := step(t, parent, Options{Submodules: &on}, &Snapshot{r0.Root, r0.Record}, all)
	c, mine := step(t, parent, Options{Submodules: &on, Compact: CompactNow}, &Snapshot{r1.Root, r1.Record}, all)
	sub, err := LoadRecord(mine.Get, c.Record.Submodules[0].Record.Cid)
	if err != nil || !sub.Base || sub.Prev != nil {
		t.Fatalf("submodule record %+v %v", sub, err)
	}
	dest := filepath.Join(t.TempDir(), "r.git")
	if _, err := Restore(context.Background(), mine.Get, c.Root, dest, RestoreOptions{Recurse: true}); err != nil {
		t.Fatal(err)
	}
}

// TestCompactNeedsBlocks: asked to compact without the earlier blocks, it
// says so; on auto it writes a delta.
func TestCompactNeedsBlocks(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	all := NewBlocks()
	r0, _ := step(t, repo, Options{}, nil, all)
	commit(t, repo, 1)
	o := Options{Prev: r0.Record, PrevRoot: r0.Root, Compact: CompactNow, TempDir: t.TempDir()}
	if _, err := Archive(context.Background(), repo, o, all.Put); err == nil {
		t.Fatal("compacted without the earlier blocks")
	}
	o.Compact = "sometimes"
	if _, err := Archive(context.Background(), repo, o, all.Put); err == nil {
		t.Fatal("bad mode accepted")
	}
}

// TestCompactBorrowOddTempDir: objects only the archive has are read
// through git's alternates, whose variable splits paths on ':'.
func TestCompactBorrowOddTempDir(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	all := NewBlocks()
	r0, _ := step(t, repo, Options{}, nil, all)
	git(t, repo, "branch", "-q", "-D", "feature")
	git(t, repo, "reflog", "expire", "--expire=now", "--all")
	git(t, repo, "gc", "-q", "--prune=now")
	commit(t, repo, 1)
	r1, _ := step(t, repo, Options{}, &Snapshot{r0.Root, r0.Record}, all)
	tmp := filepath.Join(t.TempDir(), `a:b"c`)
	os.Mkdir(tmp, 0o755)
	mine := NewBlocks()
	o := Options{Prev: r1.Record, PrevRoot: r1.Root, Get: all.Get, Compact: CompactNow, TempDir: tmp}
	c, err := Archive(context.Background(), repo, o, mine.Put)
	if err != nil {
		t.Fatal(err)
	}
	dest, rr := restoreAt(t, mine, c.Root, r0.Root)
	if !rr.FromHistory || refsOf(t, dest) != refLines(r0.Record.Refs) {
		t.Fatalf("first snapshot: %+v", rr)
	}
}

// TestHistoryRepeatedLink: a hostile record naming one history block many
// times costs one read.
func TestHistoryRepeatedLink(t *testing.T) {
	bs := NewBlocks()
	e := putRecord(t, bs, HistoryEntry{Root: "bafyreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy", Refs: []Ref{}})
	reads := 0
	get := func(c cid.Cid) ([]byte, error) { reads++; return bs.Get(c) }
	r := &Record{Kind: Kind, Version: RecordVersion, ObjectFormat: "sha1", Base: true, Refs: []Ref{}}
	for range 10000 {
		r.History = append(r.History, Link{e})
	}
	if err := r.check(); err != nil {
		t.Fatal(err)
	}
	h, err := History(get, r)
	if err != nil || len(h) != 1 || reads != 1 {
		t.Fatalf("%d entries, %d reads, %v", len(h), reads, err)
	}
	r.Prev = &Link{e}
	if r.check() == nil {
		t.Fatal("a compacted record with a prev link passed")
	}
}
