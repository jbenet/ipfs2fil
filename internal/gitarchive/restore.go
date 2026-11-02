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

	depth int
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Snapshots int
	Packs     int
	Refs      int
	Skipped   []string // remotes and submodules not restored, and why
	Restored  []string // submodule repos restored, by path under dest
	LFS       int      // Git LFS objects restored
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
	chain, err := Chain(get, root)
	if err != nil {
		return nil, err
	}
	last := chain[len(chain)-1].Record
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
	res := &RestoreResult{Snapshots: len(chain)}
	for _, s := range chain {
		for _, p := range s.Record.Packs {
			if err := g.indexPack(ctx, get, p); err != nil {
				return nil, fmt.Errorf("snapshot %s: %w", s.Root, err)
			}
			res.Packs++
			logf("pack %d: %d objects", res.Packs, p.Objects)
		}
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
		for _, s := range last.Submodules {
			if s.Record == nil {
				continue
			}
			if !safeSubmoduleName(s.Name) || o.depth >= maxSubmoduleDepth {
				res.Skipped = append(res.Skipped, fmt.Sprintf("submodule %q: unsafe name or too deep", s.Name))
				continue
			}
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
