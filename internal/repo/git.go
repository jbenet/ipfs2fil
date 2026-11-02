package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
	path string

	once sync.Once
	done atomic.Bool // load ran; set after err, rec and the rest
	err  error
	car  *carRepo
	root cid.Cid
	rec  *gitarchive.Record
	made *gitarchive.Result
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

func openGit(path string) (Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if r, ok := gits.Load(abs); ok && !r.(*gitRepo).stale() {
		return r.(*gitRepo), nil
	}
	r := &gitRepo{path: abs}
	gits.Store(abs, r)
	return r, nil
}

func (r *gitRepo) Path() string     { return r.path }
func (r *gitRepo) Kind() string     { return KindGit }
func (r *gitRepo) Version() Version { return 0 }

// Close keeps the repo open: the scan, pack and push of one run share it.
func (r *gitRepo) Close() error { return nil }

func (r *gitRepo) Describe() string {
	if err := r.load(); err != nil || r.rec == nil {
		return "git repo"
	}
	snaps := "1 snapshot"
	if n := r.snapshots(); n != 1 {
		snaps = fmt.Sprintf("%d snapshots", n)
	}
	return fmt.Sprintf("git repo (%s), %d refs, %s", r.rec.ObjectFormat, len(r.rec.Refs), snaps)
}

func (r *gitRepo) snapshots() int {
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
	if r.err != nil || r.rec == nil {
		return true
	}
	ctx := context.Background()
	g, err := gitarchive.OpenRepo(ctx, GitBin, r.path)
	if err != nil {
		return true
	}
	refs, err := g.Refs(ctx)
	if err != nil || !slices.Equal(refs, r.rec.Refs) {
		return true
	}
	head, err := g.Head(ctx)
	return err != nil || head != r.rec.Head
}

// archiveDir is the repo's directory under GitDir: its base name, so people
// can find it, and a hash of its path, so two repos never share one.
func (r *gitRepo) archiveDir() string {
	h := sha256.Sum256([]byte(r.path))
	name := strings.TrimSuffix(filepath.Base(r.path), ".git")
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
	o := gitarchive.Options{Git: GitBin, TempDir: dir, Tool: "ipfs2fil"}
	files, _ := carFiles(dir)
	if len(files) > 0 {
		c, err := openCAR(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		prev := c.(*carRepo)
		r.car = prev
		root, err := r.latest(prev)
		if err != nil {
			prev.Close()
			return fmt.Errorf("%s: %w", dir, err)
		}
		if o.Prev, err = gitarchive.LoadRecord(r.verified, root); err != nil {
			prev.Close()
			return err
		}
		o.PrevRoot = root
		o.Have = func(c cid.Cid) bool { _, ok := prev.at[string(c.Hash())]; return ok }
		o.Get = r.verified
	}
	name := filepath.Join(dir, fmt.Sprintf("%04d.car", len(files)+1))
	logf("reading git repo %s\n", r.path)
	res, err := gitarchive.WriteCAR(name, func(put gitarchive.BlockSink) (*gitarchive.Result, error) {
		return gitarchive.Archive(ctx, r.path, o, put)
	})
	if err != nil {
		if r.car != nil {
			r.car.Close()
		}
		return err
	}
	r.made = res
	for _, n := range res.Notes {
		logf("  note: %s\n", n)
	}
	if !res.Unchanged {
		logf("  %d new objects, %d commits\n", res.Objects, res.Record.Commits)
		for _, t := range res.Record.Tips {
			logf("  files of %s: %s\n", t.Ref, t.Root)
		}
		if r.car != nil {
			r.car.Close()
		}
		c, err := openCAR(dir)
		if err != nil {
			return err
		}
		r.car = c.(*carRepo)
	}
	r.root, r.rec = res.Root, res.Record
	name = strings.TrimSuffix(filepath.Base(r.path), ".git")
	if name == ".git" || name == "" {
		name = filepath.Base(filepath.Dir(r.path))
	}
	r.pins = []Pin{{Root: r.root, Mode: Recursive, Name: name}}
	return nil
}

// latest finds the newest snapshot among a directory's CAR roots.
func (r *gitRepo) latest(c *carRepo) (cid.Cid, error) {
	var roots []cid.Cid
	for _, p := range c.pins {
		roots = append(roots, p.Root)
	}
	heads, _ := gitarchive.LatestOf(func(x cid.Cid) ([]byte, error) { return verifiedGet(c, x) }, roots)
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
