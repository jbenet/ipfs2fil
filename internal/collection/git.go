package collection

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A catalog can be a git repository instead of a shared folder: ipfs2fil
// keeps a clone in the collection's directory, pulls before reading it, and
// commits and pushes this machine's subfolder after changing it. Machines
// never write each other's files, so a push that loses the race only needs a
// rebase and another try.

// CloneDir is where a git catalog is checked out, in the collection's directory.
const CloneDir = "catalog"

// savedCatalog remembers the catalog given once, so later commands need no flag.
const savedCatalog = "catalog.txt"

var scpLike = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)

// IsGitURL reports whether catalog names a git repository rather than a
// folder: a URL (https://, ssh://, git://, file://), user@host:path, or a
// path ending in .git.
func IsGitURL(catalog string) bool {
	return strings.Contains(catalog, "://") || scpLike.MatchString(catalog) || strings.HasSuffix(strings.TrimRight(catalog, "/"), ".git")
}

// ResolveCatalog picks the catalog: the one given, else the one dir
// remembers. A newly given one is remembered.
func ResolveCatalog(dir, given string) (string, error) {
	path := filepath.Join(dir, savedCatalog)
	if given == "" {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return strings.TrimSpace(string(b)), err
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == given {
		return given, nil
	}
	return given, writeFile(path, []byte(given+"\n"))
}

type gitCatalog struct {
	url, dir, host string
	pulled         error // why the last pull failed, if it did
}

// who names this machine as the author of its commits and rebases, so git
// needs no user.name or user.email set up.
func (g *gitCatalog) who(args ...string) []string {
	return append([]string{"-c", "user.name=ipfs2fil on " + g.host, "-c", "user.email=ipfs2fil@" + g.host + ".invalid"}, args...)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if errors.Is(err, exec.ErrNotFound) {
		return "", errors.New("a git catalog needs git installed; install it, or use a shared folder as --catalog")
	}
	if err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// sync clones the catalog, or pulls it. A failed pull (offline, say) leaves
// the last copy to read from, and says so through pulled.
func (g *gitCatalog) sync() error {
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(g.dir), 0o755); err != nil {
			return err
		}
		if _, err := git(filepath.Dir(g.dir), "clone", "--quiet", g.url, g.dir); err != nil {
			return fmt.Errorf("cloning the catalog %s: %w", g.url, err)
		}
		return nil
	}
	if !g.hasUpstream() {
		return nil // an empty repository: nothing to pull yet
	}
	if _, err := git(g.dir, g.who("pull", "--quiet", "--rebase", "--autostash")...); err != nil {
		g.pulled = err
	}
	return nil
}

func (g *gitCatalog) hasUpstream() bool {
	_, err := git(g.dir, "rev-parse", "--verify", "--quiet", "@{upstream}")
	return err == nil
}

// commit commits the host's subfolder and pushes, rebasing onto other
// machines' pushes when it must.
func (g *gitCatalog) commit(msg string) error {
	host := g.host
	if _, err := git(g.dir, "add", "-A", "--", host); err != nil {
		return err
	}
	if _, err := git(g.dir, "diff", "--cached", "--quiet"); err == nil {
		return nil // nothing changed
	}
	if _, err := git(g.dir, g.who("commit", "--quiet", "-m", host+": "+msg)...); err != nil {
		return err
	}
	var err error
	for try := range 4 {
		if try > 0 {
			time.Sleep(time.Duration(try) * time.Second)
			if _, err = git(g.dir, g.who("pull", "--quiet", "--rebase", "--autostash")...); err != nil {
				continue
			}
		}
		branch, berr := git(g.dir, "rev-parse", "--abbrev-ref", "HEAD")
		if berr != nil {
			return berr
		}
		if _, err = git(g.dir, "push", "--quiet", "-u", "origin", strings.TrimSpace(branch)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("pushing the catalog to %s: %w (the change is committed in %s; the next run pushes it)", g.url, err, g.dir)
}
