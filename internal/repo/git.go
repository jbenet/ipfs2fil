package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/gitarchive"
)

// KindGit is a git repo, archived through gitarchive.
const KindGit = "git"

// GitDir is where git repos' archives are kept between runs, one directory
// of CARs per repo: a full archive, then one delta per run that saw new
// commits. The CLI sets it under its home directory.
var GitDir = filepath.Join(os.TempDir(), "ipfs2fil-git")

// GitBin is the git binary to run; "git" when empty.
var GitBin string

// gitRepo is a git working tree or bare repo. On first use it brings its
// archive in GitDir up to date (a delta of what is new since the last run),
// then reads that directory of CARs. Its one pin is the newest snapshot's
// record, named after the repo, which links every earlier one.
type gitRepo struct {
	path string // the repo, the folder of repos, or the URL
	url  bool   // path is an https URL, read through a clone in GitDir/clones
	set  bool   // path is a folder of repos, archived as one set

	once sync.Once
	done atomic.Bool // load ran; set after err, rec and the rest
	err  error
	car  *carRepo
	root cid.Cid
	rec  *gitarchive.Record    // one repo's newest record
	srec *gitarchive.SetRecord // a folder's newest set record
	pins []Pin
}

var gits sync.Map // abs path -> *gitRepo, so one run archives a repo once

// isGit says whether path is a git repo and not an IPFS repo kept in a git
// checkout: one with a config file and a datastore or blocks folder, or a Helia
// blockstore, is read as IPFS. (A bare git repo has a config file too, so
// config alone does not count.)
func isGit(path string) bool {
	if strings.Contains(path, "://") || !gitarchive.IsGitRepo(path) {
		return false
	}
	if _, ok := heliaBlockstore(path); ok {
		return false
	}
	// (kubo's own source has config/ and blocks/ folders: config must be a file)
	if st, err := os.Stat(filepath.Join(path, "config")); err == nil && st.Mode().IsRegular() {
		for _, d := range []string{"datastore", "blocks"} {
			if st, err := os.Stat(filepath.Join(path, d)); err == nil && st.IsDir() {
				return false
			}
		}
	}
	return true
}

// gitHosts are forges whose https URLs name git repos, never a kubo RPC API.
var gitHosts = map[string]bool{"github.com": true, "gitlab.com": true, "codeberg.org": true, "bitbucket.org": true}

// IsGitURL says whether path is a git repo to clone: git+https://host/path,
// an https URL ending in .git, or an https URL on a known forge. Other
// http(s) URLs are kubo RPC addresses.
func IsGitURL(path string) bool {
	if !gitarchive.IsCloneURL(path) || !strings.HasPrefix(path, "git+https://") && !strings.HasPrefix(path, "https://") {
		return false
	}
	if strings.HasPrefix(path, "git+") {
		return true
	}
	u, err := url.Parse(path)
	if err != nil {
		return false
	}
	p := strings.TrimSuffix(u.Path, "/")
	return strings.HasSuffix(p, ".git") || gitHosts[strings.ToLower(u.Hostname())] && strings.Count(strings.Trim(p, "/"), "/") >= 1
}

// IsGitRemote says whether path is a git repo to clone: IsGitURL, or any
// other https URL whose server answers git's smart HTTP protocol.
func IsGitRemote(path string) bool {
	return IsGitURL(path) || servesGit(path)
}

// gitProbe is the client servesGit asks with; tests swap it.
var gitProbe = &http.Client{Timeout: 10 * time.Second}

var probed sync.Map // URL -> bool

// servesGit asks an https URL for its git refs, as git clone would first:
// a git server answers with the upload-pack advertisement, a kubo RPC API
// (which takes POSTs under /api/v0) does not.
func servesGit(path string) bool {
	if !strings.HasPrefix(path, "https://") || !gitarchive.IsCloneURL(path) {
		return false
	}
	if v, ok := probed.Load(path); ok {
		return v.(bool)
	}
	u, err := url.Parse(path)
	if err != nil || u.User != nil || u.RawQuery != "" {
		return false
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/info/refs"
	u.RawQuery = "service=git-upload-pack"
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "git/2.0 (ipfs2fil)")
	req.Header.Set("Git-Protocol", "version=2")
	ok := false
	if resp, err := gitProbe.Do(req); err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		ok = resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-git-upload-pack-advertisement")
	}
	probed.Store(path, ok)
	return ok
}

// isGitFolder says whether path is a folder of git repos: not a repo
// itself, not an IPFS repo or a folder of CARs, with a git repo under it.
func isGitFolder(path string) bool {
	if strings.Contains(path, "://") {
		return false
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() || gitarchive.IsGitRepo(path) || isCAR(path) || broadFolder(path) {
		return false
	}
	if _, ok := heliaBlockstore(path); ok {
		return false
	}
	if _, err := DetectVersion(path); err == nil {
		return false
	}
	found, _, err := gitarchive.FindRepos(path)
	return err == nil && len(found) > 0
}

// broadFolder is a folder too broad to archive every repo under by
// accident: the filesystem root or the home folder, which hold repos the
// user may never mean to publish.
func broadFolder(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	abs = filepath.Clean(abs)
	if abs == filepath.Dir(abs) {
		return true // / or C:\
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return abs == filepath.Clean(home) || abs == filepath.Dir(filepath.Clean(home))
}

func openGit(path string) (Repo, error) {
	return openGitAs(path, false)
}

func openGitAs(path string, set bool) (Repo, error) {
	key, isURL := path, strings.Contains(path, "://")
	if isURL {
		if u, err := url.Parse(strings.TrimPrefix(path, "git+")); err == nil && u.User != nil {
			return nil, fmt.Errorf("%s: ipfs2fil lists repos in its catalog, so it takes no user name or token in a URL", gitarchive.StripCredentials(strings.TrimPrefix(path, "git+")))
		}
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		key = abs
	}
	if r, ok := gits.Load(key); ok && !r.(*gitRepo).stale() {
		return r.(*gitRepo), nil
	}
	r := &gitRepo{path: key, url: isURL, set: set}
	gits.Store(key, r)
	return r, nil
}

func (r *gitRepo) Path() string     { return r.path }
func (r *gitRepo) Kind() string     { return KindGit }
func (r *gitRepo) Version() Version { return 0 }

// Close keeps the repo open: the scan, pack and push of one run share it.
func (r *gitRepo) Close() error { return nil }

func (r *gitRepo) Describe() string {
	what := "git repo"
	if r.set {
		what = "folder of git repos"
	}
	if err := r.load(); err != nil || (r.rec == nil && r.srec == nil) {
		return what
	}
	snaps := "1 snapshot"
	if n := r.snapshots(); n != 1 {
		snaps = fmt.Sprintf("%d snapshots", n)
	}
	if r.set {
		return fmt.Sprintf("folder of %d git repos, %s", len(r.srec.Repos), snaps)
	}
	return fmt.Sprintf("git repo (%s), %d refs, %s", r.rec.ObjectFormat, len(r.rec.Refs), snaps)
}

func (r *gitRepo) snapshots() int {
	if r.set {
		n := 0
		for c := r.root; n < 1<<16; n++ {
			s, err := gitarchive.LoadSet(r.verified, c)
			if err != nil {
				return n
			}
			if s.Prev == nil {
				return n + 1
			}
			c = s.Prev.Cid
		}
		return n
	}
	ch, err := gitarchive.Chain(r.verified, r.root)
	if err != nil {
		return 0
	}
	return len(ch)
}

// stale reports whether the repo's refs or HEAD moved since it was archived,
// so a long-lived process that opens it again gets a new snapshot.
func (r *gitRepo) stale() bool {
	if !r.done.Load() {
		return false
	}
	if r.err != nil || r.rec == nil && r.srec == nil {
		return true
	}
	if r.url {
		return false // asking the server costs a fetch; a new process fetches
	}
	if !r.set {
		return moved(r.path, r.rec)
	}
	found, _, err := gitarchive.FindRepos(r.path)
	if err != nil || len(found) != len(r.srec.Repos) {
		return true
	}
	for i, m := range r.srec.Repos {
		if found[i].Name != m.Name {
			return true
		}
		rec, err := gitarchive.LoadRecord(r.verified, m.Record.Cid)
		if err != nil || moved(found[i].Path, rec) {
			return true
		}
	}
	return false
}

// moved says whether the repo at path has refs or a HEAD other than rec's.
func moved(path string, rec *gitarchive.Record) bool {
	ctx := context.Background()
	g, err := gitarchive.OpenRepo(ctx, GitBin, path)
	if err != nil {
		return true
	}
	refs, err := g.Refs(ctx)
	if err != nil || !slices.Equal(refs, rec.Refs) {
		return true
	}
	head, err := g.Head(ctx)
	return err != nil || head != rec.Head
}

// archiveDir is the repo's directory under GitDir: its base name, so people
// can find it, and a hash of its path, so two repos never share one.
func (r *gitRepo) archiveDir() string {
	h := sha256.Sum256([]byte(r.path))
	name := strings.TrimSuffix(filepath.Base(r.path), ".git")
	if r.url {
		name = path.Base(gitarchive.URLName(r.path))
	}
	if name == "" || name == "." || name == string(filepath.Separator) {
		name = "repo"
	}
	return filepath.Join(GitDir, safeName(name)+"-"+hex.EncodeToString(h[:6]))
}

func (r *gitRepo) load() error {
	r.once.Do(func() { r.err = r.update(context.Background()); r.done.Store(true) })
	return r.err
}

// update writes a delta CAR of what changed since the newest snapshot in
// the repo's archive directory (a full one the first time), then opens the
// directory.
func (r *gitRepo) update(ctx context.Context) error {
	dir := r.archiveDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src := r.path
	if r.url {
		var err error
		if src, err = r.mirror(ctx, filepath.Base(dir)); err != nil {
			return err
		}
	}
	o := gitarchive.Options{Git: GitBin, TempDir: dir, Tool: "ipfs2fil"}
	var prevRoot cid.Cid
	files, _ := carFiles(dir)
	if len(files) > 0 {
		c, err := openCAR(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		prev := c.(*carRepo)
		r.car = prev
		if prevRoot, err = r.latest(prev); err != nil {
			prev.Close()
			return fmt.Errorf("%s: %w", dir, err)
		}
		o.Have = func(c cid.Cid) bool { _, ok := prev.at[string(c.Hash())]; return ok }
		o.Get = r.verified
	}
	name := filepath.Join(dir, fmt.Sprintf("%04d.car", len(files)+1))
	logf("reading git %s %s\n", map[bool]string{false: "repo", true: "repos under"}[r.set], gitarchive.StripCredentials(r.path))
	var root cid.Cid
	unchanged := false
	if r.set {
		so := gitarchive.SetOptions{Options: o, Log: func(f string, a ...any) { logf("  "+f, a...) }}
		if prevRoot.Defined() {
			p, err := gitarchive.LoadSet(r.verified, prevRoot)
			if err != nil {
				r.car.Close()
				return fmt.Errorf("%s: %w", dir, err)
			}
			so.Prev, so.PrevRoot = p, prevRoot
		}
		ins, notes, err := gitarchive.FindRepos(r.path)
		if err == nil && len(ins) == 0 {
			err = fmt.Errorf("no git repos under %s", r.path)
		}
		if err != nil {
			if r.car != nil {
				r.car.Close()
			}
			return err
		}
		for _, n := range notes {
			logf("  note: %s\n", n)
		}
		res, err := gitarchive.ArchiveSetCAR(ctx, name, ins, so)
		if err != nil {
			if r.car != nil {
				r.car.Close()
			}
			return err
		}
		for _, n := range res.Notes {
			logf("  note: %s\n", n)
		}
		for _, m := range res.Repos {
			if m.Result != nil && !m.Unchanged {
				logf("  %s: %d new objects, %d commits\n", m.Name, m.Objects, m.Record.Commits)
			}
		}
		root, r.srec, unchanged = res.Root, res.Record, res.Unchanged
	} else {
		if prevRoot.Defined() {
			var err error
			if o.Prev, err = gitarchive.LoadRecord(r.verified, prevRoot); err != nil {
				r.car.Close()
				return err
			}
			o.PrevRoot = prevRoot
		}
		res, err := gitarchive.WriteCAR(name, func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
			return gitarchive.Archive(ctx, src, o, put)
		})
		if err != nil {
			if r.car != nil {
				r.car.Close()
			}
			return err
		}
		for _, n := range res.Notes {
			logf("  note: %s\n", n)
		}
		if !res.Unchanged {
			logf("  %d new objects, %d commits\n", res.Objects, res.Record.Commits)
			for _, t := range res.Record.Tips {
				logf("  files of %s: %s\n", t.Ref, t.Root)
			}
		}
		root, r.rec, unchanged = res.Root, res.Record, res.Unchanged
	}
	if !unchanged {
		if r.car != nil {
			r.car.Close()
		}
		c, err := openCAR(dir)
		if err != nil {
			return err
		}
		r.car = c.(*carRepo)
	}
	r.root = root
	r.pins = []Pin{{Root: r.root, Mode: Recursive, Name: r.pinName()}}
	return nil
}

// pinName is the repo's or folder's name; a URL's last path part.
func (r *gitRepo) pinName() string {
	if r.url {
		return path.Base(gitarchive.URLName(r.path))
	}
	name := strings.TrimSuffix(filepath.Base(r.path), ".git")
	if name == ".git" || name == "" {
		name = filepath.Base(filepath.Dir(r.path))
	}
	return name
}

// mirror clones a URL repo into GitDir/clones the first time and fetches
// into that clone after, so each run downloads only what is new.
func (r *gitRepo) mirror(ctx context.Context, name string) (string, error) {
	clones := filepath.Join(GitDir, "clones")
	dst := filepath.Join(clones, name+".git")
	if _, err := os.Stat(filepath.Join(dst, "HEAD")); err == nil {
		logf("fetching %s\n", r.path)
		if err := gitarchive.Fetch(ctx, GitBin, dst, r.path); err != nil {
			if ctx.Err() != nil {
				return "", err
			}
			// The last clone is still a true copy of what the server had;
			// archive that and say why nothing new came.
			logf("  note: %v; archiving the last fetched state\n", err)
		}
		return dst, nil
	}
	if err := os.MkdirAll(clones, 0o755); err != nil {
		return "", err
	}
	logf("cloning %s\n", r.path)
	tmp, err := gitarchive.Clone(ctx, GitBin, r.path, clones)
	if err != nil {
		return "", err
	}
	os.RemoveAll(dst)
	if err := os.Rename(tmp, dst); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return dst, nil
}

// latest finds the newest snapshot among a directory's CAR roots.
func (r *gitRepo) latest(c *carRepo) (cid.Cid, error) {
	var roots []cid.Cid
	for _, p := range c.pins {
		roots = append(roots, p.Root)
	}
	heads, _ := gitarchive.LatestRoot(func(x cid.Cid) ([]byte, error) { return verifiedGet(c, x) }, roots)
	switch len(heads) {
	case 1:
		return heads[0], nil
	case 0:
		return cid.Undef, fmt.Errorf("no git archive snapshot among its CARs")
	}
	sort.Slice(heads, func(i, j int) bool { return heads[i].String() < heads[j].String() })
	return cid.Undef, fmt.Errorf("%d unrelated snapshots; move the extra CARs out", len(heads))
}

func verifiedGet(c *carRepo, x cid.Cid) ([]byte, error) {
	b, err := c.Get(context.Background(), x.Hash())
	if err != nil {
		return nil, err
	}
	got, err := x.Prefix().Sum(b)
	if err != nil {
		return nil, err
	}
	if !got.Equals(x) {
		return nil, fmt.Errorf("block %s does not match its hash", x)
	}
	return b, nil
}

func (r *gitRepo) verified(x cid.Cid) ([]byte, error) { return verifiedGet(r.car, x) }

func (r *gitRepo) ForEachBlock(ctx context.Context, fn func(mh multihash.Multihash, size int) error) error {
	if err := r.load(); err != nil {
		return err
	}
	return r.car.ForEachBlock(ctx, fn)
}

func (r *gitRepo) Get(ctx context.Context, mh multihash.Multihash) ([]byte, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.car.Get(ctx, mh)
}

func (r *gitRepo) Pins(ctx context.Context) ([]Pin, error) {
	if err := r.load(); err != nil {
		return nil, err
	}
	return r.pins, nil
}

func (r *gitRepo) MFSRoot(ctx context.Context) (cid.Cid, bool, error) { return cid.Undef, false, nil }
