package gitarchive

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
)

// IsCloneURL says whether s names a remote repo to clone rather than a
// local path: https://host/path, or git+https://host/path, the spelling
// that never reads as a kubo RPC address.
func IsCloneURL(s string) bool {
	_, err := cloneURL(s)
	return err == nil
}

// cloneURL is the https URL git clones for s, or why s is not one. Only
// https is followed: http can be tampered with on the way, file:// and
// local paths could name any repo on this machine, and ext:: and fd::
// run commands.
func cloneURL(s string) (string, error) {
	raw := strings.TrimPrefix(s, "git+")
	u, err := url.Parse(raw)
	if err != nil || !cloneSchemes[u.Scheme] || (u.Host == "" && u.Scheme != "file") || u.Opaque != "" {
		return "", fmt.Errorf("%s is not an https URL", s)
	}
	if strings.HasPrefix(raw, "-") || strings.Contains(raw, "::") || strings.ContainsAny(raw, "\x00\n\r\t ") {
		return "", fmt.Errorf("%s is not a URL git should clone", s)
	}
	return raw, nil
}

// URLName names a cloned repo after its URL: host and path, without
// .git, a trailing slash, credentials, query or fragment, e.g.
// github.com/ipfs/kubo.
func URLName(s string) string {
	raw, err := cloneURL(s)
	if err != nil {
		return ""
	}
	u, _ := url.Parse(raw)
	p := strings.TrimSuffix(strings.Trim(path.Clean("/"+u.Path), "/"), ".git")
	if p == "" || p == "." {
		return u.Hostname()
	}
	return u.Hostname() + "/" + p
}

// Clone makes a bare clone of an https URL (or git+https) in a new
// directory under tmp, which the caller removes. Branches and tags come
// along, as git clone brings them; the repo's own settings stay
// untrusted (archive it with Options.UntrustedConfig). Git runs no hooks
// and asks for no credentials: a token in the URL is all it gets.
func Clone(ctx context.Context, bin, s, tmp string) (string, error) {
	raw, err := cloneURL(s)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(tmp, "git-ipfs-archive-clone-*")
	if err != nil {
		return "", err
	}
	cl := &Git{Bin: bin}
	u, _ := url.Parse(raw)
	args := []string{"-c", "protocol." + u.Scheme + ".allow=always", "clone", "-q", "--bare", "--no-recurse-submodules", "--", raw, dir}
	if _, err := cl.run(ctx, nil, args...); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("cloning %s: %w", StripCredentials(raw), stripURL(err, raw))
	}
	// The clone's origin keeps the URL as given; a token in it would be
	// recorded as a remote and stay on disk. Record it without.
	if clean := StripCredentials(raw); clean != raw {
		g := &Git{Bin: bin, Dir: dir}
		if _, err := g.run(ctx, nil, "remote", "set-url", "origin", clean); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

// stripURL keeps a token out of an error message.
func stripURL(err error, raw string) error {
	clean := StripCredentials(raw)
	if clean == raw {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), raw, clean))
}

// Fetch brings a bare clone made by Clone up to date with its URL:
// branches and tags as the server has them now, deleted ones pruned.
func Fetch(ctx context.Context, bin, dir, s string) error {
	raw, err := cloneURL(s)
	if err != nil {
		return err
	}
	u, _ := url.Parse(raw)
	g := &Git{Bin: bin, Dir: dir}
	_, err = g.run(ctx, nil, "-c", "protocol."+u.Scheme+".allow=always", "fetch", "-q", "--prune", "--no-recurse-submodules", "--", raw,
		"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	if err != nil {
		return fmt.Errorf("fetching %s: %w", StripCredentials(raw), stripURL(err, raw))
	}
	return nil
}
