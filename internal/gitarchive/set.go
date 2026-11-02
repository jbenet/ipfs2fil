package gitarchive

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ipfs/go-cid"
)

// SetKind names a set record: many repos archived together, such as every
// repo under a folder. Each member has its own record; forks are packed
// thin against the members before them (Record.Bases).
const SetKind = "git-archive-set"

// SetVersion is the set record layout this package writes and reads.
const SetVersion = 1

// SetRecord is the root of a set snapshot. Prev is the set snapshot before
// it; each member's record builds on that set's record of the same name.
type SetRecord struct {
	Kind    string      `cbor:"kind"`
	Version int         `cbor:"version"`
	Prev    *Link       `cbor:"prev,omitempty"`
	Repos   []SetMember `cbor:"repos"`
	Tool    string      `cbor:"tool,omitempty"`
}

// SetMember is one repo in a set: a name (its path under the folder, with
// slashes, or host/path for a URL) and its record.
type SetMember struct {
	Name   string `cbor:"name"`
	Record Link   `cbor:"record"`
	// Note says why this snapshot kept an older record for the repo, such
	// as an error reading it this time.
	Note string `cbor:"note,omitempty"`
}

// maxSetRepos bounds the members of one set.
const maxSetRepos = 1 << 14

// DecodeSet parses and checks a set record block.
func DecodeSet(b []byte) (*SetRecord, error) {
	var s SetRecord
	if err := decMode.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("git archive set: %w", err)
	}
	if s.Kind != SetKind {
		return nil, fmt.Errorf("not a git archive set (kind %q)", s.Kind)
	}
	if s.Version != SetVersion {
		return nil, fmt.Errorf("git archive set version %d; this build reads %d", s.Version, SetVersion)
	}
	return &s, s.check()
}

func (s *SetRecord) check() error {
	if len(s.Repos) > maxSetRepos {
		return fmt.Errorf("set names %d repos, more than %d", len(s.Repos), maxSetRepos)
	}
	seen := map[string]bool{}
	for _, m := range s.Repos {
		if !safeMemberName(m.Name) {
			return fmt.Errorf("set member %q has an unsafe name", m.Name)
		}
		if seen[m.Name] {
			return fmt.Errorf("set names %q twice", m.Name)
		}
		seen[m.Name] = true
		if m.Record.Type() != cid.DagCBOR {
			return fmt.Errorf("set member %s: record is not DAG-CBOR", m.Name)
		}
	}
	return nil
}

// safeMemberName is a relative slash path that stays inside a restore's
// destination: no empty, . or .. parts, no backslashes or control bytes.
func safeMemberName(name string) bool {
	if !safeSubmoduleName(name) || len(name) > 4096 {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// LoadSet decodes the set record at c.
func LoadSet(get BlockGetter, c cid.Cid) (*SetRecord, error) {
	if c.Type() != cid.DagCBOR {
		return nil, fmt.Errorf("%s is not a DAG-CBOR record", c)
	}
	data, err := get(c)
	if err != nil {
		return nil, err
	}
	return DecodeSet(data)
}

// IsSet says whether the block at c decodes as a set record.
func IsSet(get BlockGetter, c cid.Cid) bool {
	_, err := LoadSet(get, c)
	return err == nil
}

// Input is one repo to archive in a set: a name and a local path (a clone
// of a URL is a local path by then).
type Input struct {
	Name string
	Path string
	// Untrusted marks a repo someone else controls, such as a clone:
	// its ConfigFile cannot turn on settings that cost much more.
	Untrusted bool
}

// FindRepos lists the git repos under dir, named by their path under it
// with slashes, sorted. It does not look inside a repo it found
// (submodules are --submodules' job) and does not follow symlinks. notes
// says what it skipped.
func FindRepos(dir string) (repos []Input, notes []string, err error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			notes = append(notes, fmt.Sprintf("skipped %s: %v", p, err))
			return fs.SkipDir
		}
		if !d.IsDir() {
			if d.Type()&fs.ModeSymlink != 0 && IsGitRepo(p) {
				notes = append(notes, fmt.Sprintf("skipped %s: a symlink (not followed)", p))
			}
			return nil
		}
		if d.Name() == ".git" && p != root {
			return fs.SkipDir // a working tree's git dir is found through its parent
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Count(rel, string(filepath.Separator)) >= maxFindDepth {
			return fs.SkipDir
		}
		if !IsGitRepo(p) {
			return nil
		}
		name := filepath.ToSlash(rel)
		if p == root {
			name = filepath.Base(root)
		}
		if !safeMemberName(name) {
			notes = append(notes, fmt.Sprintf("skipped %s: its name is not one an archive can hold", p))
			return fs.SkipDir
		}
		repos = append(repos, Input{Name: name, Path: p})
		if len(repos) > maxSetRepos {
			return fmt.Errorf("more than %d git repos under %s", maxSetRepos, dir)
		}
		return fs.SkipDir
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, notes, nil
}

// maxFindDepth is how many folders deep FindRepos looks.
const maxFindDepth = 8

// SetOptions steer ArchiveSet.
type SetOptions struct {
	Options // for every member; Prev, Bases and Instructions per member come from the set

	// Prev is the set snapshot to build on and PrevRoot its CID; each
	// member builds on Prev's record of the same name, read through Get.
	Prev     *SetRecord
	PrevRoot cid.Cid

	// Log, when set, is told as each repo starts.
	Log func(format string, args ...any)
}

// SetResult is what ArchiveSet made.
type SetResult struct {
	Root      cid.Cid
	Record    *SetRecord
	Unchanged bool // every member and the list match Prev: Root is PrevRoot
	Repos     []MemberResult
	Notes     []string
}

// MemberResult is one member's Result, or the error that kept it out.
type MemberResult struct {
	Name string
	*Result
	Bases []string // the members it was packed against
	Err   error
}

// ArchiveSet archives each input as a member of one set and puts the set
// record last, as the root. Members are archived in order; each is packed
// thin against earlier members that share a root commit with it (forks), so
// objects they share are stored once. A member that fails is left out with
// a note, or keeps its record from Prev.
func ArchiveSet(ctx context.Context, inputs []Input, o SetOptions, put BlockSink) (*SetResult, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	names := map[string]bool{}
	for _, in := range inputs {
		if !safeMemberName(in.Name) {
			return nil, fmt.Errorf("repo name %q is not one an archive can hold", in.Name)
		}
		if names[in.Name] {
			return nil, fmt.Errorf("two repos are named %q", in.Name)
		}
		names[in.Name] = true
	}
	if len(inputs) > maxSetRepos {
		return nil, fmt.Errorf("%d repos, more than %d", len(inputs), maxSetRepos)
	}
	prev := map[string]SetMember{}
	if o.Prev != nil {
		if !o.PrevRoot.Defined() {
			return nil, errors.New("a previous set record needs its CID")
		}
		for _, m := range o.Prev.Repos {
			prev[m.Name] = m
		}
	}
	put = dedup(put, o.Have)
	set := &SetRecord{Kind: SetKind, Version: SetVersion, Tool: o.Tool}
	res := &SetResult{Record: set}
	type done struct {
		name  string
		base  Base
		roots map[string]bool
	}
	var earlier []done
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		logf("%s\n", in.Name)
		mo := o.Options
		mo.Prev, mo.PrevRoot, mo.Bases = nil, cid.Undef, nil
		// Members write deltas only for now: a set's blocks are deduped
		// across its members and earlier CARs, so a compacted member
		// would not stand alone.
		mo.Compact = CompactNever
		mo.UntrustedConfig = o.UntrustedConfig || in.Untrusted
		pm, hadPrev := prev[in.Name]
		if hadPrev && o.Get != nil {
			if r, err := LoadRecord(o.Get, pm.Record.Cid); err == nil {
				mo.Prev, mo.PrevRoot = r, pm.Record.Cid
			} else {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: its last snapshot is unreadable (%v); archived in full", in.Name, err))
			}
		}
		mr := MemberResult{Name: in.Name}
		g, err := OpenRepo(ctx, o.Git, in.Path)
		var roots map[string]bool
		if err == nil {
			roots, err = g.rootCommits(ctx)
		}
		if err == nil {
			for _, e := range earlier {
				if e.base.Record.ObjectFormat == g.Format.Name && shareAny(roots, e.roots) {
					mo.Bases = append(mo.Bases, e.base)
				}
			}
			for _, b := range usableBases(g, mo.Bases) {
				for _, e := range earlier {
					if e.base.Root == b.Root {
						mr.Bases = append(mr.Bases, e.name)
					}
				}
			}
			mr.Result, err = archiveRepo(ctx, in.Path, mo, put, 0, map[string]bool{})
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrCollision) {
				return nil, fmt.Errorf("%s: %w", in.Name, err)
			}
			mr.Err = err
			res.Repos = append(res.Repos, mr)
			if hadPrev {
				set.Repos = append(set.Repos, SetMember{Name: in.Name, Record: pm.Record, Note: "kept the last snapshot: " + firstLine(err.Error())})
				res.Notes = append(res.Notes, fmt.Sprintf("%s: %v; kept its last snapshot", in.Name, err))
			} else {
				res.Notes = append(res.Notes, fmt.Sprintf("%s: %v; left out", in.Name, err))
			}
			continue
		}
		for _, n := range mr.Notes {
			res.Notes = append(res.Notes, in.Name+": "+n)
		}
		res.Repos = append(res.Repos, mr)
		set.Repos = append(set.Repos, SetMember{Name: in.Name, Record: Link{mr.Root}})
		if len(mr.Record.Shallow) == 0 {
			earlier = append(earlier, done{in.Name, Base{Root: mr.Root, Record: mr.Record, Dir: g.Dir}, roots})
		}
	}
	for _, m := range o.prevOnly(names) {
		res.Notes = append(res.Notes, fmt.Sprintf("%s: no longer here; the previous snapshot still holds it", m.Name))
	}
	if o.Prev != nil {
		set.Prev = &Link{o.PrevRoot}
		if slices.EqualFunc(o.Prev.Repos, set.Repos, func(a, b SetMember) bool { return a == b }) {
			return &SetResult{Root: o.PrevRoot, Record: o.Prev, Unchanged: true, Repos: res.Repos, Notes: res.Notes}, nil
		}
	}
	if err := set.check(); err != nil {
		return nil, err
	}
	c, b, err := encodeBlock(set)
	if err != nil {
		return nil, err
	}
	if err := put(c, b); err != nil {
		return nil, err
	}
	res.Root = c
	return res, nil
}

func (o SetOptions) prevOnly(names map[string]bool) []SetMember {
	if o.Prev == nil {
		return nil
	}
	var out []SetMember
	for _, m := range o.Prev.Repos {
		if !names[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

func shareAny(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// maxRootCommits bounds the root commits read per repo; a repo with more
// (grafted histories) is matched on the first ones.
const maxRootCommits = 64

// rootCommits lists the commits without parents that the repo's refs
// reach: forks of one project share them.
func (g *Git) rootCommits(ctx context.Context) (map[string]bool, error) {
	out, err := g.run(ctx, nil, "rev-list", "--max-parents=0", "--all")
	if err != nil {
		if refs, rerr := g.Refs(ctx); rerr == nil && len(refs) == 0 {
			return map[string]bool{}, nil // no commits yet
		}
		return nil, err
	}
	m := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		if len(m) == maxRootCommits {
			break
		}
		m[l] = true
	}
	return m, nil
}

// ArchiveSetCAR is ArchiveSet into a CAR file at out; nothing is written
// when nothing changed.
func ArchiveSetCAR(ctx context.Context, out string, inputs []Input, o SetOptions) (*SetResult, error) {
	var sr *SetResult
	_, err := WriteCAR(out, func(put BlockSink) (*Result, error) {
		var err error
		if sr, err = ArchiveSet(ctx, inputs, o, put); err != nil {
			return nil, err
		}
		return &Result{Root: sr.Root, Unchanged: sr.Unchanged}, nil
	})
	return sr, err
}

// RestoreDir is where RestoreSet puts a member: dest/<name>.git, as bare
// repos are named, unless the name ends in .git already.
func RestoreDir(dest, name string) string {
	if !strings.HasSuffix(name, ".git") {
		name += ".git"
	}
	return filepath.Join(dest, filepath.FromSlash(name))
}

// RestoreSet restores every member of the set at root (or those only names)
// into dest, each a bare repo at RestoreDir(dest, name).
func RestoreSet(ctx context.Context, get BlockGetter, root cid.Cid, dest string, only []string, o RestoreOptions) (map[string]*RestoreResult, error) {
	s, err := LoadSet(get, root)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	if err := emptyDir(dest); err != nil {
		return nil, err
	}
	// Check every name first, so a bad one restores nothing: each must be
	// asked for, and no two may share a folder (names that differ only in
	// case do on some disks) or nest one inside the other.
	var picked []SetMember
	dirs := map[string]string{} // lower-cased folder -> name
	filter := len(want) > 0
	for _, m := range s.Repos {
		if filter && !want[m.Name] {
			continue
		}
		delete(want, m.Name)
		picked = append(picked, m)
		d := strings.ToLower(RestoreDir(dest, m.Name))
		if other, ok := dirs[d]; ok {
			return nil, fmt.Errorf("%s and %s would restore to the same folder", other, m.Name)
		}
		dirs[d] = m.Name
	}
	for n := range want {
		return nil, fmt.Errorf("the set has no repo named %q", n)
	}
	for d, n := range dirs {
		for p := filepath.Dir(d); len(p) > len(dest); p = filepath.Dir(p) {
			if other, ok := dirs[p]; ok {
				return nil, fmt.Errorf("%s would restore inside %s", n, other)
			}
		}
	}
	out := map[string]*RestoreResult{}
	for _, m := range picked {
		d := RestoreDir(dest, m.Name)
		if err := insideNewDirs(dest, d); err != nil {
			return nil, err
		}
		r, err := Restore(ctx, get, m.Record.Cid, d, o)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Name, err)
		}
		out[m.Name] = r
	}
	return out, nil
}

// insideNewDirs refuses a member folder whose parents already exist as
// something other than plain folders this restore made (a symlink planted
// by an earlier member could point outside dest).
func insideNewDirs(dest, d string) error {
	rel, err := filepath.Rel(dest, d)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s is outside %s", d, dest)
	}
	p := dest
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		p = filepath.Join(p, part)
		if st, err := os.Lstat(p); err == nil && !st.IsDir() {
			return fmt.Errorf("%s is not a folder", p)
		}
	}
	return nil
}

// LatestRoot picks the newest snapshot among roots, of repos or of sets:
// the one no other names as Prev, or lists in a compacted snapshot's
// history.
func LatestRoot(get BlockGetter, roots []cid.Cid) ([]cid.Cid, error) {
	var recs []cid.Cid
	prev := map[string]bool{}
	for _, c := range roots {
		if r, err := LoadRecord(get, c); err == nil {
			recs = append(recs, c)
			if r.Prev != nil {
				prev[r.Prev.KeyString()] = true
			}
			hist, _ := History(get, r)
			for _, e := range hist {
				if hc, err := cid.Decode(e.Root); err == nil {
					prev[hc.KeyString()] = true
				}
			}
		} else if s, err := LoadSet(get, c); err == nil {
			recs = append(recs, c)
			if s.Prev != nil {
				prev[s.Prev.KeyString()] = true
			}
		}
	}
	var heads []cid.Cid
	seen := map[string]bool{}
	for _, c := range recs {
		if !prev[c.KeyString()] && !seen[c.KeyString()] {
			heads = append(heads, c)
			seen[c.KeyString()] = true
		}
	}
	return heads, nil
}
