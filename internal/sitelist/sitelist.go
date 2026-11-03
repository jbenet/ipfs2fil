// Package sitelist reads a site list, a small file naming websites that
// live on IPFS, finds the root CID each one serves now, and keeps what it
// found in a lock file next to the list.
//
// A list is TOML, named <name>.ipfs-sites.toml (or just ipfs-sites.toml),
// so a folder can hold many lists:
//
//	title = "My sites"
//
//	[[site]]
//	source = "benet.ai"                          # a domain with DNSLink
//
//	[[site]]
//	source = "https://fil.pub/"                  # a URL: its host's DNSLink, else the X-Ipfs-Path its server sends
//
//	[[site]]
//	source = "vitalik.eth"                       # an ENS name
//
//	[[site]]
//	source = "/ipns/k51qzi5uqu5d..."             # an IPNS name; its signed record is checked
//	name = "blog"
//	note = "my blog"
//
//	[[site]]
//	source = "/ipfs/bafybei..."                  # a fixed version, never looked up again
//	name = "launch-2019"
//
// What each site resolved to goes in <name>.ipfs-sites.lock.toml (see
// Lock), rewritten on each run.
//
// The older line format (a .sites file, one site per line with an
// optional name=) is still read; see ParseText.
//
// A site's name is how it is pinned and shown: by default the domain, the
// ENS name, or the IPNS or IPFS identifier. Names are unique in a list.
package sitelist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/ipfs/go-cid"
)

// Kinds of site, by how its root is found.
const (
	KindDNSLink = "dnslink" // a domain: its _dnslink TXT record, else its server's X-Ipfs-Path
	KindURL     = "url"     // an https URL: the same, for its host
	KindENS     = "ens"     // name.eth: through an ENS gateway
	KindIPNS    = "ipns"    // /ipns/<key>: the signed record, from a gateway
	KindIPFS    = "ipfs"    // /ipfs/<cid>: fixed
)

// Site is one site of a list.
type Site struct {
	Name   string `json:"name" toml:"name"`
	Source string `json:"source" toml:"source"` // as written
	Kind   string `json:"kind" toml:"kind"`
	Note   string `json:"note,omitempty" toml:"note,omitempty"`
	Host   string `json:"-" toml:"-"` // the domain, ENS name, IPNS key or CID
	Line   int    `json:"-" toml:"-"` // in a line-format list
}

// maxSites bounds a list.
const maxSites = 10000

// ParseText reads a site list in the older line format (a .sites file):
// one site per line, where it lives and then an optional name=. Everything
// after a # that starts a line or follows a space is a comment.
//
//	benet.ai
//	/ipns/k51qzi5uqu5d...  name=blog
func ParseText(r io.Reader) ([]Site, error) {
	var out []Site
	names := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		if i := strings.Index(line, "\t#"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		s, err := parseSource(f[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		s.Line = n
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			switch {
			case !ok:
				return nil, fmt.Errorf("line %d: %q is not key=value", n, kv)
			case k == "name":
				if err := checkName(v); err != nil {
					return nil, fmt.Errorf("line %d: %w", n, err)
				}
				s.Name = v
			default:
				return nil, fmt.Errorf("line %d: unknown field %q (only name=)", n, k)
			}
		}
		if prev, ok := names[s.Name]; ok {
			return nil, fmt.Errorf("line %d: the name %q is taken by line %d; give one of them name=", n, s.Name, prev)
		}
		names[s.Name] = n
		out = append(out, s)
		if len(out) > maxSites {
			return nil, fmt.Errorf("over %d sites", maxSites)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("the list names no sites")
	}
	return out, nil
}

// checkName allows names that are safe as file and folder names.
func checkName(v string) error {
	if v == "" || len(v) > 128 || v == "." || v == ".." {
		return fmt.Errorf("bad name %q", v)
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-+@", r)) {
			return fmt.Errorf("bad name %q: use letters, digits and . _ - + @", v)
		}
	}
	return nil
}

func parseSource(src string) (Site, error) {
	s := Site{Source: src}
	switch {
	case strings.HasPrefix(src, "/ipfs/") || strings.HasPrefix(src, "ipfs://"):
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(src, "/ipfs/"), "ipfs://"), "/")
		if strings.Contains(id, "/") {
			return s, fmt.Errorf("%s: a path inside a CID is not a site root", src)
		}
		c, err := cid.Decode(id)
		if err != nil {
			return s, fmt.Errorf("%s: %w", src, err)
		}
		s.Kind, s.Host, s.Name = KindIPFS, c.String(), c.String()
	case strings.HasPrefix(src, "/ipns/") || strings.HasPrefix(src, "ipns://"):
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(src, "/ipns/"), "ipns://"), "/")
		if id == "" || strings.Contains(id, "/") {
			return s, fmt.Errorf("%s: name an IPNS key or domain, without a path", src)
		}
		switch {
		case strings.HasSuffix(strings.ToLower(id), ".eth"):
			s.Kind, s.Host = KindENS, strings.ToLower(id)
		case isDomain(id):
			s.Kind, s.Host = KindDNSLink, strings.ToLower(id)
		default:
			s.Kind, s.Host = KindIPNS, id
		}
		s.Name = s.Host
	case strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "http://"):
		u, err := url.Parse(src)
		if err != nil || u.Hostname() == "" {
			return s, fmt.Errorf("%s: not a URL", src)
		}
		if u.User != nil || u.RawQuery != "" || strings.Trim(u.Path, "/") != "" {
			return s, fmt.Errorf("%s: name a site's root, with no path, query or user", src)
		}
		s.Kind, s.Host, s.Name = KindURL, strings.ToLower(u.Hostname()), strings.ToLower(u.Hostname())
		if strings.HasSuffix(s.Host, ".eth") {
			s.Kind = KindENS
		}
	case strings.HasSuffix(strings.ToLower(src), ".eth"):
		s.Kind, s.Host, s.Name = KindENS, strings.ToLower(src), strings.ToLower(src)
	case isDomain(src):
		s.Kind, s.Host, s.Name = KindDNSLink, strings.ToLower(strings.TrimSuffix(src, ".")), strings.ToLower(strings.TrimSuffix(src, "."))
	default:
		return s, fmt.Errorf("%q is not a domain, URL, ENS name, /ipns/ or /ipfs/ path", src)
	}
	return s, nil
}

// isDomain says whether s looks like a DNS name with a dot.
func isDomain(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if !strings.Contains(s, ".") || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}
