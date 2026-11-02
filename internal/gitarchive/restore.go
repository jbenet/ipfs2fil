package gitarchive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ipfs/go-cid"
)

// RestoreOptions steer Restore.
type RestoreOptions struct {
	Git     string
	Strict  bool // git fsck --strict, which some old repos fail
	Remotes bool // also configure the recorded remotes
	// Recurse restores archived submodules too, each in modules/<name>
	// under its parent, where git keeps a checked-out submodule's repo.
	Recurse bool
	Log     func(format string, args ...any)
	// At restores an earlier snapshot of the archive at root instead: by
	// its own blocks when they are there, else from the compacted
	// snapshot whose history lists it.
	At cid.Cid

	depth int
	subs  *int // submodule repos restored so far, across the whole tree
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Snapshots int
	Packs     int
	Refs      int
	Skipped   []string // remotes and submodules not restored, and why
	// FromHistory means the snapshot restored was one a compacted snapshot
	// replaced: its refs are exact, and the repo may hold objects that
	// later snapshots added too.
	FromHistory bool
	Restored    []string // submodule repos restored, by path under dest
	LFS         int      // Git LFS objects restored
}

// Restore rebuilds the repo of the snapshot at root as a bare repo in dest,
// which must not exist or be empty: every pack in order (thin ones
// completed by git index-pack --fix-thin), then refs, HEAD and the shallow
// list, then git fsck. Every ref must end up naming its recorded object.
func Restore(ctx context.Context, get BlockGetter, root cid.Cid, dest string, o RestoreOptions) (*RestoreResult, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	chain, last, fromHist, err := restorePlan(get, root, o.At)
	if err != nil {
		return nil, err
	}
	f, _ := ParseFormat(last.ObjectFormat)
	if err := emptyDir(dest); err != nil {
		return nil, err
	}
	init := &Git{Bin: o.Git}
	if _, err := init.run(ctx, nil, "init", "-q", "--bare", "--object-format="+f.Name, dest); err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(dest)
	g := &Git{Bin: o.Git, Dir: abs, Format: f}
	res := &RestoreResult{Snapshots: len(chain), FromHistory: fromHist}
	packs, based, err := packsToIndex(get, chain)
	if err != nil {
		return nil, err
	}
	for _, p := range packs {
		if err := g.indexPack(ctx, get, p.Pack); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", p.root, err)
		}
		res.Packs++
		logf("pack %d: %d objects", res.Packs, p.Objects)
	}
	if len(last.Shallow) > 0 {
		if err := os.WriteFile(filepath.Join(abs, "shallow"), []byte(strings.Join(last.Shallow, "\n")+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	var upd bytes.Buffer
	for _, r := range last.Refs {
		fmt.Fprintf(&upd, "create %s %s\n", r.Name, r.ID)
	}
	if upd.Len() > 0 {
		if _, err := g.run(ctx, upd.Bytes(), "update-ref", "--stdin"); err != nil {
			return nil, err
		}
	}
	res.Refs = len(last.Refs)
	switch {
	case strings.HasPrefix(last.Head, "refs/"):
		if _, err := g.run(ctx, nil, "symbolic-ref", "HEAD", last.Head); err != nil {
			return nil, err
		}
	case last.Head != "":
		if _, err := g.run(ctx, nil, "update-ref", "--no-deref", "HEAD", last.Head); err != nil {
			return nil, err
		}
	}
	if o.Remotes {
		for _, r := range last.Remotes {
			if why := unsafeRemote(r); why != "" {
				res.Skipped = append(res.Skipped, fmt.Sprintf("remote %s (%s): %s", r.Name, r.URL, why))
				continue
			}
			if _, err := g.run(ctx, nil, "remote", "add", "--", r.Name, r.URL); err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("remote %s: %v", r.Name, err))
			}
		}
	}
	if based {
		// The bases' packs brought in their repos' objects too; keep only
		// what this repo's refs reach, as the repo itself had.
		if _, err := g.run(ctx, nil, "repack", "-a", "-d", "-q", "--no-write-bitmap-index"); err != nil {
			return nil, err
		}
		if _, err := g.run(ctx, nil, "prune", "--expire=now"); err != nil {
			return nil, err
		}
	}
	args := []string{"fsck", "--full", "--no-dangling", "--no-progress"}
	if o.Strict {
		args = append(args, "--strict")
	}
	if _, err := g.run(ctx, nil, args...); err != nil {
		return nil, fmt.Errorf("restored repo fails fsck: %w", err)
	}
	if err := g.checkRefs(ctx, last.Refs); err != nil {
		return nil, err
	}
	if res.LFS, err = restoreLFS(get, chain, abs); err != nil {
		return nil, err
	}
	if o.Recurse {
		if o.subs == nil {
			o.subs = new(int)
		}
		for _, s := range last.Submodules {
			if s.Record == nil {
				continue
			}
			if !safeSubmoduleName(s.Name) || o.depth >= maxSubmoduleDepth {
				res.Skipped = append(res.Skipped, fmt.Sprintf("submodule %q: unsafe name or too deep", s.Name))
				continue
			}
			if *o.subs >= maxSubmoduleRestores {
				return nil, fmt.Errorf("submodule %s: the archive names more than %d submodule repos", s.Path, maxSubmoduleRestores)
			}
			*o.subs++
			sub := filepath.Join("modules", filepath.FromSlash(s.Name))
			so := o
			so.depth++
			sr, err := Restore(ctx, get, s.Record.Cid, filepath.Join(abs, sub), so)
			if err != nil {
				return nil, fmt.Errorf("submodule %s: %w", s.Path, err)
			}
			logf("submodule %s: %d refs", s.Path, sr.Refs)
			res.Restored = append(res.Restored, filepath.ToSlash(sub))
			for _, r := range sr.Restored {
				res.Restored = append(res.Restored, filepath.ToSlash(sub)+"/"+r)
			}
			res.Skipped = append(res.Skipped, sr.Skipped...)
		}
	}
	return res, nil
}

// restorePlan picks the snapshots whose packs to index and the record whose
// refs to write. With at, that is the part of root's chain up to at; else,
// when root is a compacted snapshot's descendant whose history names at,
// the compacted snapshot and a record made from that history entry; else
// at's own chain, if its blocks are there. History comes before at's own
// blocks: after compaction, an earlier snapshot's records may outlive its
// packs.
func restorePlan(get BlockGetter, root, at cid.Cid) ([]Snapshot, *Record, bool, error) {
	chain, err := Chain(get, root)
	if err != nil {
		return nil, nil, false, err
	}
	if !at.Defined() || at.Equals(root) {
		return chain, chain[len(chain)-1].Record, false, nil
	}
	for i, s := range chain {
		if s.Root.Equals(at) {
			return chain[:i+1], s.Record, false, nil
		}
	}
	base := chain[0]
	hist, herr := History(get, base.Record)
	for _, e := range hist {
		if e.Root != at.String() {
			continue
		}
		r := &Record{Kind: Kind, Version: RecordVersion, ObjectFormat: base.Record.ObjectFormat,
			Head: e.Head, Refs: e.Refs, Shallow: e.Shallow, Remotes: e.Remotes, Submodules: e.Submodules}
		if err := r.check(); err != nil {
			return nil, nil, false, fmt.Errorf("history entry %s: %w", e.Root, err)
		}
		return chain[:1], r, true, nil
	}
	if own, err := Chain(get, at); err == nil {
		return own, own[len(own)-1].Record, false, nil
	}
	if herr != nil {
		return nil, nil, false, fmt.Errorf("reading the compacted history: %w", herr)
	}
	return nil, nil, false, fmt.Errorf("snapshot %s is not in the archive at %s, nor in its compacted history", at, root)
}

// maxBaseRecords bounds how many base snapshots one restore follows.
const maxBaseRecords = 4096

type packAt struct {
	Pack
	root cid.Cid // the snapshot naming it
}

// packsToIndex lists the packs a restore of chain needs, in an order where
// every thin pack comes after the packs holding its bases: for each
// snapshot, its bases' chains (recursively), then its own packs. A pack
// named twice is listed once. based says whether any base was followed.
func packsToIndex(get BlockGetter, chain []Snapshot) ([]packAt, bool, error) {
	var out []packAt
	packs := map[string]bool{}
	visited := map[string]bool{} // base snapshots already followed
	based := false
	loaded := 0 // base snapshots read, counting repeats
	var walk func(chain []Snapshot, depth int) error
	walk = func(chain []Snapshot, depth int) error {
		for _, s := range chain {
			for _, b := range s.Record.Bases {
				if visited[b.KeyString()] {
					continue
				}
				visited[b.KeyString()] = true
				based = true
				if depth >= maxSubmoduleDepth*2 || len(visited) > maxBaseRecords {
					return fmt.Errorf("snapshot %s: bases nest too deep or too many", s.Root)
				}
				bc, err := Chain(get, b.Cid)
				if err != nil {
					return fmt.Errorf("base %s: %w", b.Cid, err)
				}
				if loaded += len(bc); loaded > maxChain {
					return fmt.Errorf("snapshot %s: its bases' chains hold more than %d snapshots", s.Root, maxChain)
				}
				if bc[0].Record.ObjectFormat != chain[0].Record.ObjectFormat {
					return fmt.Errorf("base %s is a %s repo", b.Cid, bc[0].Record.ObjectFormat)
				}
				for _, x := range bc {
					visited[x.Root.KeyString()] = true // its chain is walked now
				}
				if err := walk(bc, depth+1); err != nil {
					return err
				}
			}
			for _, p := range s.Record.Packs {
				if packs[p.Pack.KeyString()] {
					continue
				}
				packs[p.Pack.KeyString()] = true
				out = append(out, packAt{p, s.Root})
			}
		}
		return nil
	}
	err := walk(chain, 0)
	return out, based, err
}

// indexPack streams one stored pack into git index-pack.
func (g *Git) indexPack(ctx context.Context, get BlockGetter, p Pack) error {
	c := g.cmd(ctx, "index-pack", "--stdin", "--fix-thin")
	pr, pw := io.Pipe()
	c.Stdin = pr
	var errb bytes.Buffer
	c.Stdout, c.Stderr = io.Discard, &errb
	if err := c.Start(); err != nil {
		return err
	}
	n, rerr := readFile(get, p.Pack.Cid, pw)
	pw.CloseWithError(rerr)
	werr := c.Wait()
	if rerr != nil {
		return fmt.Errorf("reading pack %s: %w", p.Pack.Cid, rerr)
	}
	if werr != nil {
		return fmt.Errorf("git index-pack: %s", strings.TrimSpace(errb.String()))
	}
	if n != p.Size {
		return fmt.Errorf("pack %s is %d bytes, record says %d", p.Pack.Cid, n, p.Size)
	}
	return nil
}

// checkRefs confirms every ref names what the record says.
func (g *Git) checkRefs(ctx context.Context, want []Ref) error {
	have, err := g.Refs(ctx)
	if err != nil {
		return err
	}
	m := map[string]string{}
	for _, r := range have {
		m[r.Name] = r.ID
	}
	for _, r := range want {
		if m[r.Name] != r.ID {
			return fmt.Errorf("ref %s is %q after restore, record says %s", r.Name, m[r.Name], r.ID)
		}
	}
	if len(have) != len(want) {
		return fmt.Errorf("restored repo has %d refs, record has %d", len(have), len(want))
	}
	return nil
}

func emptyDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("%s is not empty", dir)
	}
	return nil
}

// unsafeRemote says why a recorded remote should not be configured: git
// runs commands for ext:: and fd:: URLs, and a remote name can carry options.
func unsafeRemote(r Remote) string {
	if r.Name == "" || strings.HasPrefix(r.Name, "-") || strings.ContainsAny(r.Name, " \t\n\x00/\\") {
		return "odd name"
	}
	if strings.HasPrefix(r.URL, "-") {
		return "URL starts with -"
	}
	if strings.Contains(r.URL, "::") {
		return "transport helper URLs can run commands"
	}
	if u, err := url.Parse(r.URL); err == nil && u.Scheme != "" && len(u.Scheme) > 1 {
		switch u.Scheme {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
			return ""
		}
		return "scheme " + u.Scheme + " is not allowed"
	}
	return "" // scp-like host:path, or a local path
}
