package gitarchive

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
)

// setFolder makes a folder of repos: upstream, a fork of it with its own
// commit (and without upstream's newest), a shallow clone, and an
// unrelated repo, plus a non-repo folder and a nested repo inside a
// working tree that must not be listed.
func setFolder(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	up := newRepo(t, "")
	git(t, dir, "clone", "-q", "--no-local", up, filepath.Join(dir, "upstream"))
	git(t, dir, "clone", "-q", "--no-local", up, filepath.Join(dir, "forks", "fork"))
	fork := filepath.Join(dir, "forks", "fork")
	write(t, filepath.Join(fork, "fork.txt"), "the fork's own change\n")
	git(t, fork, "add", "-A")
	git(t, fork, "commit", "-q", "-m", "fork work")
	// Upstream moves on after the fork was made.
	upc := filepath.Join(dir, "upstream")
	write(t, filepath.Join(upc, "later.txt"), "only upstream has this\n")
	write(t, filepath.Join(upc, "big.txt"), strings.Repeat("line of text that repeats in every version\n", 400)+"upstream's version\n")
	git(t, upc, "add", "-A")
	git(t, upc, "commit", "-q", "-m", "upstream later")
	git(t, dir, "clone", "-q", "--depth=1", "--no-local", "file://"+up, filepath.Join(dir, "shallow"))
	other := filepath.Join(dir, "other")
	os.MkdirAll(other, 0o755)
	git(t, other, "init", "-q", "-b", "main")
	write(t, filepath.Join(other, "x"), "x\n")
	git(t, other, "add", "-A")
	git(t, other, "commit", "-q", "-m", "unrelated")
	nested := filepath.Join(other, "vendor", "nested")
	os.MkdirAll(nested, 0o755)
	git(t, nested, "init", "-q")
	os.MkdirAll(filepath.Join(dir, "notes"), 0o755)
	write(t, filepath.Join(dir, "notes", "readme.txt"), "not a repo\n")
	return dir
}

func archiveSetT(t *testing.T, ins []Input, prev *SetResult, bs *Blocks) *SetResult {
	t.Helper()
	o := SetOptions{Options: Options{TempDir: t.TempDir()}}
	if prev != nil {
		o.Prev, o.PrevRoot = prev.Record, prev.Root
		o.Have, o.Get = bs.Has, bs.Get
	}
	res, err := ArchiveSet(context.Background(), ins, o, bs.Put)
	if err != nil {
		t.Fatal(err)
	}
	bs.roots = append(bs.roots, res.Root)
	return res
}

func member(t *testing.T, r *SetResult, name string) MemberResult {
	t.Helper()
	for _, m := range r.Repos {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no member %s", name)
	return MemberResult{}
}

func objectsIn(t *testing.T, dir string) string {
	return git(t, dir, "rev-list", "--objects", "--all")
}

func TestSetOfForks(t *testing.T) {
	needGit(t)
	dir := setFolder(t)
	ins, notes, err := FindRepos(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range ins {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, " "); got != "forks/fork other shallow upstream" {
		t.Fatalf("found %q (notes %v)", got, notes)
	}
	bs := NewBlocks()
	res := archiveSetT(t, ins, nil, bs)

	// The fork comes first by name; upstream is packed against it.
	fork, up := member(t, res, "forks/fork"), member(t, res, "upstream")
	if len(fork.Record.Bases) != 0 || fork.Record.Version != RecordVersion {
		t.Fatalf("first member has bases: %+v", fork.Record.Bases)
	}
	if len(up.Record.Bases) != 1 || up.Record.Bases[0].Cid != fork.Root || up.Record.Version != RecordVersionBases {
		t.Fatalf("upstream bases %v, version %d", up.Record.Bases, up.Record.Version)
	}
	if up.Objects > 5 || !up.Record.Packs[0].Thin || up.PackBytes > 2000 {
		t.Fatalf("upstream stored %d objects in %d bytes (thin %v); want only its newer commit, tree and blobs, big.txt as a delta", up.Objects, up.PackBytes, up.Record.Packs[0].Thin)
	}
	if sh := member(t, res, "shallow"); len(sh.Record.Bases) != 0 {
		t.Fatal("a shallow repo was packed against another")
	}
	if o := member(t, res, "other"); len(o.Record.Bases) != 0 {
		t.Fatal("an unrelated repo was packed against another")
	}

	// Restore every member: refs and objects match, and upstream holds
	// none of the fork's own objects.
	dest := filepath.Join(t.TempDir(), "out")
	rs, err := RestoreSet(context.Background(), bs.Get, res.Root, dest, nil, RestoreOptions{Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 4 {
		t.Fatalf("restored %d repos", len(rs))
	}
	for _, in := range ins {
		r := RestoreDir(dest, in.Name)
		if refsOf(t, r) != refsOf(t, in.Path) {
			t.Fatalf("%s: refs differ", in.Name)
		}
		if in.Name != "shallow" && objectsIn(t, r) != objectsIn(t, in.Path) {
			t.Fatalf("%s: objects differ", in.Name)
		}
	}
	upr := RestoreDir(dest, "upstream")
	if n := git(t, upr, "count-objects", "-v"); !strings.Contains(n, "count: 0") {
		t.Fatalf("loose objects left: %s", n)
	}
	forkHead := git(t, filepath.Join(dir, "forks", "fork"), "rev-parse", "HEAD")
	if _, err := (&Git{Dir: upr}).run(context.Background(), nil, "cat-file", "-e", forkHead); err == nil {
		t.Fatal("restored upstream holds the fork's commit")
	}

	// One member alone, through its record: the base comes along.
	one := filepath.Join(t.TempDir(), "one.git")
	if _, err := Restore(context.Background(), bs.Get, up.Root, one, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if refsOf(t, one) != refsOf(t, filepath.Join(dir, "upstream")) {
		t.Fatal("member restore refs differ")
	}

	// Nothing changed: no new set.
	again := archiveSetT(t, ins, res, bs)
	if !again.Unchanged || again.Root != res.Root {
		t.Fatal("unchanged folder made a new set")
	}

	// The fork moves on: a delta for it alone, then both restore.
	fp := filepath.Join(dir, "forks", "fork")
	write(t, filepath.Join(fp, "fork2.txt"), "more\n")
	git(t, fp, "add", "-A")
	git(t, fp, "commit", "-q", "-m", "fork more")
	d := archiveSetT(t, ins, res, bs)
	if d.Unchanged || d.Record.Prev == nil || d.Record.Prev.Cid != res.Root {
		t.Fatal("delta set does not point back")
	}
	if f := member(t, d, "forks/fork"); f.Unchanged || f.Record.Prev == nil || f.Objects > 4 {
		t.Fatalf("fork delta: %+v", f.Result)
	}
	if u := member(t, d, "upstream"); !u.Unchanged {
		t.Fatal("upstream should be unchanged")
	}
	latest, err := bs.Latest()
	if err != nil || latest != d.Root {
		t.Fatalf("latest %s %v", latest, err)
	}
	dest2 := filepath.Join(t.TempDir(), "out")
	if _, err := RestoreSet(context.Background(), bs.Get, d.Root, dest2, []string{"forks/fork", "upstream"}, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if refsOf(t, RestoreDir(dest2, "forks/fork")) != refsOf(t, fp) {
		t.Fatal("fork refs differ after delta")
	}
	if _, err := os.Stat(RestoreDir(dest2, "other")); err == nil {
		t.Fatal("--only restored another repo")
	}
	dest3 := filepath.Join(t.TempDir(), "out")
	if _, err := RestoreSet(context.Background(), bs.Get, d.Root, dest3, []string{"forks/fork"}, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(dest3); len(ents) != 1 {
		t.Fatalf("--only one repo restored %d", len(ents))
	}

	// A repo that goes away is noted; one that breaks keeps its last record.
	os.RemoveAll(filepath.Join(dir, "other"))
	os.WriteFile(filepath.Join(dir, "shallow", ".git", "HEAD"), []byte("garbage\n"), 0o644)
	ins2 := []Input{}
	for _, in := range ins {
		if in.Name != "other" {
			ins2 = append(ins2, in)
		}
	}
	g := archiveSetT(t, ins2, d, bs)
	var kept bool
	for _, m := range g.Record.Repos {
		if m.Name == "shallow" && strings.HasPrefix(m.Note, "kept the last snapshot") && m.Record.Cid == member(t, d, "shallow").Root {
			kept = true
		}
	}
	if !kept || !strings.Contains(strings.Join(g.Notes, "\n"), "other: no longer here") {
		t.Fatalf("set after a removal and a break: %+v %v", g.Record.Repos, g.Notes)
	}
}

// A set record is checked: member names stay inside the restore folder.
func TestSetRecordCheck(t *testing.T) {
	needGit(t)
	bs := NewBlocks()
	res := archive(t, newRepo(t, ""), nil, bs)
	for _, name := range []string{"../x", "/abs", "a/../../b", "a\\b", "", "a//b", "x\x01y"} {
		c := putRecord(t, bs, SetRecord{Kind: SetKind, Version: SetVersion, Repos: []SetMember{{Name: name, Record: Link{res.Root}}}})
		if _, err := LoadSet(bs.Get, c); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	c := putRecord(t, bs, SetRecord{Kind: SetKind, Version: SetVersion, Repos: []SetMember{{Name: "a", Record: Link{res.Root}}, {Name: "a", Record: Link{res.Root}}}})
	if _, err := LoadSet(bs.Get, c); err == nil {
		t.Error("duplicate name accepted")
	}
	// Two names that differ only in case would restore to one folder on
	// a case-insensitive disk.
	c = putRecord(t, bs, SetRecord{Kind: SetKind, Version: SetVersion, Repos: []SetMember{{Name: "A", Record: Link{res.Root}}, {Name: "a", Record: Link{res.Root}}}})
	if _, err := RestoreSet(context.Background(), bs.Get, c, t.TempDir(), nil, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "same folder") {
		t.Errorf("case clash: %v", err)
	}
	// One repo restored inside another's git dir.
	dest := t.TempDir()
	c = putRecord(t, bs, SetRecord{Kind: SetKind, Version: SetVersion, Repos: []SetMember{{Name: "a", Record: Link{res.Root}}, {Name: "a.git/hooks", Record: Link{res.Root}}}})
	if _, err := RestoreSet(context.Background(), bs.Get, c, dest, nil, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Errorf("nested: %v", err)
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 0 {
		t.Error("a refused set restored something")
	}
}

// Records whose bases name each other, or a record whose version does not
// match its bases, do not loop or restore half a repo.
func TestBasesHostile(t *testing.T) {
	needGit(t)
	bs := NewBlocks()
	res := archive(t, newRepo(t, ""), nil, bs)
	// Version 1 with bases: an older reader would restore without them.
	r := *res.Record
	r.Bases = []Link{{res.Root}}
	c := putRecord(t, bs, r)
	if _, err := LoadRecord(bs.Get, c); err == nil {
		t.Error("version 1 record with bases accepted")
	}
	// A base that names itself through Bases ends.
	r.Version = RecordVersionBases
	self := putRecord(t, bs, r) // bases: the plain full snapshot
	r2 := *res.Record
	r2.Version = RecordVersionBases
	r2.Bases = []Link{{self}}
	r2.Tool = "loop"
	c2 := putRecord(t, bs, r2)
	r3 := r2
	r3.Bases = []Link{{c2}, {self}, {c2}}
	r3.Tool = "loop2"
	c3 := putRecord(t, bs, r3)
	packs, based, err := packsToIndex(bs.Get, []Snapshot{{c3, &r3}})
	if err != nil || !based || len(packs) != 1 {
		t.Fatalf("packs %d, based %v, err %v", len(packs), based, err)
	}
	// Too many bases is refused at decode.
	r4 := r2
	for i := 0; i < maxBases+1; i++ {
		r4.Bases = append(r4.Bases, Link{self})
	}
	if _, err := LoadRecord(bs.Get, putRecord(t, bs, r4)); err == nil {
		t.Error("too many bases accepted")
	}
	// A base of another object format is refused.
	r5 := *res.Record
	r5.ObjectFormat = "sha256"
	r5.Refs, r5.Head, r5.Log, r5.Packs = nil, "", nil, nil
	r5.Tips = nil
	b5 := putRecord(t, bs, r5)
	r6 := r2
	r6.Bases = []Link{{b5}}
	if _, _, err := packsToIndex(bs.Get, []Snapshot{{cid.Undef, &r6}}); err == nil {
		t.Error("base of another format accepted")
	}
}

// A clone URL becomes a bare clone whose config is untrusted; a token in
// the URL is not kept in its remote, and local or command URLs are refused.
func TestCloneURL(t *testing.T) {
	needGit(t)
	for s, want := range map[string]bool{
		"https://github.com/ipfs/kubo":     true,
		"git+https://github.com/ipfs/kubo": true,
		"http://github.com/ipfs/kubo":      false,
		"file:///etc":                      false,
		"ext::sh -c touch% /tmp/pwned":     false,
		"https://":                         false,
		"/home/me/repo":                    false,
		"git+ssh://git@github.com/x/y":     false,
		"https://-oProxyCommand=x/y":       true, // a host, after --; git refuses it
	} {
		if IsCloneURL(s) != want {
			t.Errorf("IsCloneURL(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]string{
		"https://github.com/ipfs/kubo.git":            "github.com/ipfs/kubo",
		"git+https://u:tok@github.com/ipfs/kubo/?x=1": "github.com/ipfs/kubo",
		"https://example.com":                         "example.com",
		"https://example.com/a/../../b":               "example.com/b",
	} {
		if got := URLName(s); got != want {
			t.Errorf("URLName(%q) = %q, want %q", s, got, want)
		}
	}

	src := newRepo(t, "")
	write(t, filepath.Join(src, ConfigFile), "[archive]\n\ttips = all\n\tsubmodules = true\n")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-q", "-m", "config")
	cloneSchemes["file"] = true
	defer delete(cloneSchemes, "file")
	dir, err := Clone(context.Background(), "", "file://"+src, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if refsOf(t, dir) == "" {
		t.Fatal("empty clone")
	}
	bs := NewBlocks()
	res, err := Archive(context.Background(), dir, Options{TempDir: t.TempDir(), UntrustedConfig: true}, bs.Put)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Record.Tips) > 1 || res.Record.Settings["archive.submodules"] == "true" {
		t.Fatalf("untrusted config turned on costly settings: %v %v", res.Record.Settings, res.Notes)
	}
}

// A previous set whose member record is missing archives that member in
// full, with a note, instead of failing every repo.
func TestSetPrevMemberMissing(t *testing.T) {
	needGit(t)
	repo := newRepo(t, "")
	other := NewBlocks()
	gone := archive(t, newRepo(t, ""), nil, other) // its blocks are not in bs
	bs := NewBlocks()
	prev := &SetResult{Record: &SetRecord{Kind: SetKind, Version: SetVersion, Repos: []SetMember{{Name: "r", Record: Link{gone.Root}}}}}
	c := putRecord(t, bs, prev.Record)
	prev.Root = c
	res := archiveSetT(t, []Input{{Name: "r", Path: repo}}, prev, bs)
	m := member(t, res, "r")
	if m.Err != nil || m.Record.Prev != nil || !strings.Contains(strings.Join(res.Notes, "\n"), "archived in full") {
		t.Fatalf("member %+v, notes %v", m, res.Notes)
	}
}
