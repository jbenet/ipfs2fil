package gitarchive

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ipfs/go-cid"
)

// Submodule is a submodule HEAD's tree names: where it goes, where it comes
// from, and the commit the parent pins. Record links the submodule's own
// archive when submodules are archived (--submodules); by default only
// this is kept, and the parent's packs hold the commit id.
type Submodule struct {
	Name   string `cbor:"name"`
	Path   string `cbor:"path"`
	URL    string `cbor:"url,omitempty"`
	Commit string `cbor:"commit"`
	Record *Link  `cbor:"record,omitempty"`
	// Note says why a submodule asked for was not archived, or what it lacks.
	Note string `cbor:"note,omitempty"`
}

// maxSubmoduleDepth bounds how deep submodules of submodules go.
const maxSubmoduleDepth = 8

// cloneSchemes are the URL schemes a submodule missing locally may be
// cloned from. A local path or file:// URL in .gitmodules could name any
// repo on this machine, private ones included, so they are not followed;
// ext:: and fd:: run commands. Tests widen it.
var cloneSchemes = map[string]bool{"https": true}

// submodules lists the submodules at HEAD: gitlinks in its tree, with their
// names and URLs from its .gitmodules.
func (g *Git) submodules(ctx context.Context) ([]Submodule, error) {
	if _, err := g.run(ctx, nil, "rev-parse", "-q", "--verify", "HEAD^{tree}"); err != nil {
		return nil, nil // no commits
	}
	out, err := g.run(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", "HEAD")
	if err != nil {
		return nil, err
	}
	var subs []Submodule
	for _, e := range bytes.Split(out, []byte{0}) {
		meta, p, ok := bytes.Cut(e, []byte("\t"))
		f := strings.Fields(string(meta))
		if !ok || len(f) != 3 || f[0] != "160000" {
			continue
		}
		subs = append(subs, Submodule{Path: string(p), Commit: f[2]})
	}
	if len(subs) == 0 {
		return nil, nil
	}
	names := map[string]string{} // path -> name
	urls := map[string]string{}  // name -> url
	if cfg, err := g.run(ctx, nil, "config", "--blob", "HEAD:.gitmodules", "--no-includes", "--null", "--list"); err == nil {
		for _, kv := range bytes.Split(cfg, []byte{0}) {
			key, val, _ := strings.Cut(string(kv), "\n")
			rest, ok := strings.CutPrefix(key, "submodule.")
			if !ok {
				continue
			}
			i := strings.LastIndex(rest, ".")
			if i < 0 {
				continue
			}
			name, field := rest[:i], rest[i+1:]
			switch field {
			case "path":
				names[val] = name
			case "url":
				urls[name] = val
			}
		}
	}
	for i := range subs {
		s := &subs[i]
		s.Name = names[s.Path]
		if s.Name == "" {
			s.Name = s.Path // git's default
		}
		s.URL = StripCredentials(urls[s.Name])
		if !utf8.ValidString(s.Name) || !utf8.ValidString(s.Path) {
			return nil, fmt.Errorf("submodule %q has a name or path that is not UTF-8", s.Path)
		}
	}
	return subs, nil
}

// safeSubmoduleName refuses names that would leave modules/ when used as a
// path, as git itself does (CVE-2018-11235).
func safeSubmoduleName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\x00\n\\") || filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// archiveSubmodules archives each submodule's repo: from the parent's
// modules/<name> when it is there, else a bare clone of its URL. Each gets
// its own record; prev's records for the same names are built on.
func archiveSubmodules(ctx context.Context, g *Git, subs []Submodule, o Options, put BlockSink, depth int, seen map[string]bool) ([]string, error) {
	var notes []string
	prev := map[string]*Link{}
	if o.Prev != nil {
		for _, s := range o.Prev.Submodules {
			if s.Record != nil {
				prev[s.Name] = s.Record
			}
		}
	}
	for i := range subs {
		s := &subs[i]
		if !safeSubmoduleName(s.Name) {
			s.Note = "unsafe name; not archived"
			notes = append(notes, fmt.Sprintf("submodule %q: unsafe name; not archived", s.Name))
			continue
		}
		if depth >= maxSubmoduleDepth {
			s.Note = "too deep; not archived"
			notes = append(notes, fmt.Sprintf("submodule %s: nested more than %d deep; not archived", s.Path, maxSubmoduleDepth))
			continue
		}
		path := filepath.Join(g.Dir, "modules", filepath.FromSlash(s.Name))
		cloned := false
		if _, err := os.Stat(path); err != nil {
			p, why, err := cloneSubmodule(ctx, g, s.URL, o.TempDir)
			if err != nil {
				return nil, err
			}
			if why != "" {
				s.Note = why
				notes = append(notes, fmt.Sprintf("submodule %s: %s", s.Path, why))
				continue
			}
			defer os.RemoveAll(p)
			path, cloned = p, true
		}
		abs, _ := filepath.Abs(path)
		if seen[abs] {
			s.Note = "already archived in this run"
			continue
		}
		seen[abs] = true
		so := o
		so.Prev, so.PrevRoot, so.Tips, so.Instructions = nil, cid.Undef, "none", nil
		so.UntrustedConfig = o.UntrustedConfig || cloned // a clone's config is someone else's
		if l := prev[s.Name]; l != nil && o.Get != nil {
			if r, err := LoadRecord(o.Get, l.Cid); err == nil {
				so.Prev, so.PrevRoot = r, l.Cid
			}
		}
		res, err := archiveRepo(ctx, path, so, put, depth+1, seen)
		if err != nil {
			return nil, fmt.Errorf("submodule %s: %w", s.Path, err)
		}
		for _, n := range res.Notes {
			notes = append(notes, fmt.Sprintf("submodule %s: %s", s.Path, n))
		}
		s.Record = &Link{res.Root}
		if !hasCommit(res.Record, s.Commit) {
			sg := &Git{Bin: g.Bin, Dir: abs, Format: g.Format}
			if _, err := sg.run(ctx, nil, "cat-file", "-e", s.Commit+"^{commit}"); err != nil {
				s.Note = "the pinned commit is not in the submodule's repo"
				notes = append(notes, fmt.Sprintf("submodule %s: commit %s is not in its repo", s.Path, s.Commit[:min(12, len(s.Commit))]))
			}
		}
	}
	return notes, nil
}

func hasCommit(r *Record, id string) bool {
	for _, ref := range r.Refs {
		if ref.ID == id || ref.Peeled == id {
			return true
		}
	}
	return false
}

// cloneSubmodule makes a bare clone of a public URL in tmp. It returns why
// not, instead, for a URL it will not follow.
func cloneSubmodule(ctx context.Context, g *Git, raw, tmp string) (string, string, error) {
	if raw == "" {
		return "", "no URL in .gitmodules and not checked out; not archived", nil
	}
	u, err := url.Parse(raw)
	if err != nil || !cloneSchemes[u.Scheme] || strings.HasPrefix(raw, "-") || strings.Contains(raw, "::") {
		return "", fmt.Sprintf("not checked out, and %s is not an https URL to clone; not archived", raw), nil
	}
	dir, err := os.MkdirTemp(tmp, "submodule-*")
	if err != nil {
		return "", "", err
	}
	cl := &Git{Bin: g.Bin}
	args := []string{"-c", "protocol." + u.Scheme + ".allow=always", "clone", "-q", "--bare", "--no-recurse-submodules", "--", raw, dir}
	if _, err := cl.run(ctx, nil, args...); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Sprintf("cloning %s failed: %v", raw, err), nil
	}
	return dir, "", nil
}
