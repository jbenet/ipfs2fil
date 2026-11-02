package gitarchive

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Git runs git plumbing against one repo. It never runs hooks, never fetches
// (a partial clone's missing objects are an error, not a download), and
// ignores GIT_* variables from the caller's environment that could point it
// at another repo.
type Git struct {
	Bin    string // git binary; "git" when empty
	Dir    string // the repo's git dir
	Format Format

	// Alternates is another objects directory git also reads from, as
	// GIT_ALTERNATE_OBJECT_DIRECTORIES: compaction uses one restored from
	// the archive for objects the repo no longer has.
	Alternates string
}

// safeConfig keeps a hostile repo config from running anything while we read.
var safeConfig = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "protocol.allow=never",
	"-c", "core.askPass=",
	"-c", "credential.helper=",
}

func (g *Git) bin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "git"
}

// env is the caller's environment minus anything that steers git elsewhere.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "GIT_EXEC_PATH=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_NO_LAZY_FETCH=1", // git 2.45+: never fetch a partial clone's missing objects
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
}

func (g *Git) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{}, safeConfig...)
	if g.Dir != "" {
		full = append(full, "--git-dir="+g.Dir)
	}
	full = append(full, args...)
	c := exec.CommandContext(ctx, g.bin(), full...)
	c.Env = gitEnv()
	if g.Alternates != "" {
		c.Env = append(c.Env, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+g.Alternates)
	}
	return c
}

// run runs git and returns stdout, or an error carrying stderr.
func (g *Git) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	c := g.cmd(ctx, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out.Bytes(), nil
}

// OpenRepo finds the git dir for path (a working tree or a bare repo) and
// reads its object format. It refuses partial clones, whose missing objects
// would need a fetch.
func OpenRepo(ctx context.Context, bin, path string) (*Git, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", path)
	}
	probe := &Git{Bin: bin}
	c := probe.cmd(ctx, "-C", abs, "rev-parse", "--absolute-git-dir")
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		if _, lerr := exec.LookPath(probe.bin()); lerr != nil {
			return nil, fmt.Errorf("git is not installed (needed to read %s): %w", path, lerr)
		}
		return nil, fmt.Errorf("%s is not a git repo: %s", path, strings.TrimSpace(errb.String()))
	}
	g := &Git{Bin: bin, Dir: strings.TrimSpace(out.String())}
	fo, err := g.run(ctx, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if g.Format, err = ParseFormat(strings.TrimSpace(string(fo))); err != nil {
		return nil, err
	}
	if v, _ := g.run(ctx, nil, "config", "--get-regexp", `^(extensions\.partialclone|remote\..*\.promisor)$`); len(bytes.TrimSpace(v)) > 0 {
		return nil, fmt.Errorf("%s is a partial clone (objects are missing until fetched); run 'git fetch --refetch' or clone it fully first", path)
	}
	return g, nil
}

// IsGitRepo says whether path looks like a git working tree or bare repo,
// without running git.
func IsGitRepo(path string) bool {
	if st, err := os.Stat(filepath.Join(path, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
		return true
	}
	for _, f := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(path, f)); err != nil {
			return false
		}
	}
	head, err := os.ReadFile(filepath.Join(path, "HEAD"))
	return err == nil && (bytes.HasPrefix(head, []byte("ref: ")) || len(bytes.TrimSpace(head)) == 40 || len(bytes.TrimSpace(head)) == 64)
}

// Ref is one ref: its name, the object it names, and for an annotated tag
// the object the tag points at.
type Ref struct {
	Name   string `cbor:"name"`
	ID     string `cbor:"id"`
	Peeled string `cbor:"peeled,omitempty"`
}

// Refs lists every ref, sorted by name.
func (g *Git) Refs(ctx context.Context) ([]Ref, error) {
	out, err := g.run(ctx, nil, "for-each-ref", "--format=%(objectname) %(*objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, " ", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("odd for-each-ref line %q", line)
		}
		refs = append(refs, Ref{Name: f[2], ID: f[0], Peeled: f[1]})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Head returns what HEAD names: a ref ("refs/heads/main") or, when detached,
// an object id.
func (g *Git) Head(ctx context.Context) (string, error) {
	out, err := g.run(ctx, nil, "symbolic-ref", "-q", "HEAD")
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	out, err = g.run(ctx, nil, "rev-parse", "-q", "--verify", "HEAD")
	if err != nil {
		return "", nil // unborn and not symbolic: nothing to say
	}
	return strings.TrimSpace(string(out)), nil
}

// Remote is a configured remote and its fetch URL, with any user name or
// password taken out.
type Remote struct {
	Name string `cbor:"name"`
	URL  string `cbor:"url"`
}

// Remotes lists configured remotes.
func (g *Git) Remotes(ctx context.Context) ([]Remote, error) {
	out, err := g.run(ctx, nil, "config", "-z", "--get-regexp", `^remote\..*\.url$`)
	if err != nil {
		return nil, nil // no remotes: git config exits 1
	}
	var rs []Remote
	for _, kv := range strings.Split(string(out), "\x00") {
		k, v, ok := strings.Cut(kv, "\n")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, "remote."), ".url")
		rs = append(rs, Remote{Name: name, URL: StripCredentials(v)})
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return rs, nil
}

// StripCredentials removes a user name and password from a URL, so a token
// in https://user:token@host/... never reaches an archive.
func StripCredentials(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Scheme == "" || u.Host == "" {
		// scp-like (git@host:path) carries a login name, not a secret
		return raw
	}
	if u.Scheme == "ssh" || u.Scheme == "git+ssh" {
		u.User = url.User(u.User.Username())
		return u.String()
	}
	u.User = nil
	return u.String()
}

// Shallow lists the commits a shallow clone's history stops at, or nil.
func (g *Git) Shallow() ([]string, error) {
	b, err := os.ReadFile(filepath.Join(g.Dir, "shallow"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Fields(string(b)) {
		ids = append(ids, l)
	}
	sort.Strings(ids)
	return ids, nil
}

// Present filters ids to those the repo has.
func (g *Git) Present(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	in := strings.Join(ids, "\n") + "\n"
	out, err := g.run(ctx, []byte(in), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var have []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] != "missing" {
			have = append(have, f[0])
		}
	}
	return have, nil
}

// PackObjects writes a pack of everything reachable from tips and not from
// exclude into w. With thin, deltas may use excluded objects as bases.
func (g *Git) PackObjects(ctx context.Context, w io.Writer, tips, exclude []string, thin bool) error {
	args := []string{"pack-objects", "--revs", "--stdout", "-q", "--delta-base-offset"}
	if thin {
		args = append(args, "--thin")
	}
	var in bytes.Buffer
	for _, t := range tips {
		in.WriteString(t + "\n")
	}
	for _, x := range exclude {
		in.WriteString("^" + x + "\n")
	}
	c := g.cmd(ctx, args...)
	c.Stdin = &in
	c.Stdout = w
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		return fmt.Errorf("git pack-objects: %s", strings.TrimSpace(errb.String()+" "+err.Error()))
	}
	return nil
}

// catFile reads objects from a long-running `git cat-file --batch`.
type catFile struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

func (g *Git) catFile(ctx context.Context) (*catFile, error) {
	c := g.cmd(ctx, "cat-file", "--batch")
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	return &catFile{cmd: c, in: in, out: bufio.NewReaderSize(out, 1<<16)}, nil
}

func (cf *catFile) get(id []byte) (int, []byte, error) {
	if _, err := fmt.Fprintf(cf.in, "%x\n", id); err != nil {
		return 0, nil, err
	}
	line, err := cf.out.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	f := strings.Fields(line)
	if len(f) == 2 && f[1] == "missing" {
		return 0, nil, fmt.Errorf("object %x is missing", id)
	}
	if len(f) != 3 {
		return 0, nil, fmt.Errorf("cat-file said %q", line)
	}
	n, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || n < 0 || n > maxObject {
		return 0, nil, fmt.Errorf("cat-file said %q", line)
	}
	typ := 0
	for t, name := range typeNames {
		if name != "" && name == f[1] {
			typ = t
		}
	}
	if typ == 0 {
		return 0, nil, fmt.Errorf("object %x has type %q", id, f[1])
	}
	data := make([]byte, n+1)
	if _, err := io.ReadFull(cf.out, data); err != nil {
		return 0, nil, err
	}
	return typ, data[:n], nil
}

func (cf *catFile) close() {
	cf.in.Close()
	cf.cmd.Wait()
}
