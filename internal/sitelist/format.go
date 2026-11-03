package sitelist

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// File names. A list is <name>.ipfs-sites.toml, or ipfs-sites.toml alone;
// its lock file is the same name with .lock before .toml. An older
// line-format list ends in .sites.
const (
	Ext       = ".ipfs-sites.toml"
	LockExt   = ".ipfs-sites.lock.toml"
	Bare      = "ipfs-sites.toml"
	BareLock  = "ipfs-sites.lock.toml"
	LegacyExt = ".sites"
)

// List is a site list file.
type List struct {
	Path  string // as opened
	Title string // shown with the sites; the file's name if unset
	// Gateways replaces the gateways sites are fetched from and IPNS
	// records are asked of, for this list.
	Gateways []string
	Sites    []Site
	Legacy   bool // the older line format
	Raw      []byte
}

// IsListName says whether path is named like a site list (either format),
// without looking at the file. Lock files are not lists.
func IsListName(path string) bool {
	b := strings.ToLower(filepath.Base(path))
	if b == BareLock || strings.HasSuffix(b, LockExt) {
		return false
	}
	return b == Bare || strings.HasSuffix(b, Ext) || strings.HasSuffix(b, LegacyExt)
}

// LockPath is where the lock file of the list at path goes:
// x.ipfs-sites.toml -> x.ipfs-sites.lock.toml. A line-format list has
// none ("").
func LockPath(path string) string {
	dir, b := filepath.Split(path)
	lb := strings.ToLower(b)
	switch {
	case lb == Bare:
		return filepath.Join(dir, BareLock)
	case strings.HasSuffix(lb, Ext):
		return filepath.Join(dir, b[:len(b)-len(Ext)]+LockExt)
	}
	return ""
}

// ListName is the list's name: its file name without the extension
// (mine.ipfs-sites.toml -> mine), or for ipfs-sites.toml its folder's name
// (/x/blogs/ipfs-sites.toml -> blogs).
func ListName(path string) string {
	b := filepath.Base(path)
	lb := strings.ToLower(b)
	switch {
	case lb == Bare:
		if abs, err := filepath.Abs(path); err == nil {
			if d := filepath.Base(filepath.Dir(abs)); d != "/" && d != "." && d != string(filepath.Separator) {
				return d
			}
		}
		return "ipfs-sites"
	case strings.HasSuffix(lb, Ext):
		return b[:len(b)-len(Ext)]
	case strings.HasSuffix(lb, LegacyExt):
		return b[:len(b)-len(LegacyExt)]
	}
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// Load reads the list at path: TOML, unless it ends in .sites.
func Load(path string) (*List, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	l := &List{Path: path, Raw: b}
	lp := strings.ToLower(path)
	text := strings.HasSuffix(lp, LegacyExt)
	if !text {
		var t *List
		t, err = ParseTOML(b)
		if t != nil {
			l.Title, l.Gateways, l.Sites = t.Title, t.Gateways, t.Sites
		}
		// A file named neither way may be in the line format.
		if err != nil && !strings.HasSuffix(lp, ".toml") {
			if _, terr := ParseText(bytes.NewReader(b)); terr == nil {
				text = true
			}
		}
	}
	if text {
		l.Legacy = true
		l.Sites, err = ParseText(bytes.NewReader(b))
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Title == "" {
		l.Title = ListName(path)
	}
	return l, nil
}

// tomlList is the file's shape.
type tomlList struct {
	Title    string     `toml:"title"`
	Gateways []string   `toml:"gateways"`
	Site     []tomlSite `toml:"site"`
}

type tomlSite struct {
	Source string `toml:"source"`
	Name   string `toml:"name"`
	Note   string `toml:"note"`
}

// ParseTOML reads a list in TOML: one [[site]] table per site, so adding
// or removing a site touches only its own lines. Unknown keys are errors,
// so a typo does not go unnoticed.
func ParseTOML(b []byte) (*List, error) {
	var f tomlList
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("unknown keys: %s (a list has title, gateways, and [[site]] tables with source, name and note)", strings.Join(keys, ", "))
	}
	l := &List{Title: f.Title}
	for _, g := range f.Gateways {
		if !strings.HasPrefix(g, "https://") && !strings.HasPrefix(g, "http://") {
			return nil, fmt.Errorf("gateway %q is not an http(s) URL", g)
		}
		l.Gateways = append(l.Gateways, strings.TrimSuffix(g, "/"))
	}
	names := map[string]string{}
	for i, ts := range f.Site {
		where := fmt.Sprintf("site %d", i+1)
		if ts.Source == "" {
			return nil, fmt.Errorf("%s: no source", where)
		}
		s, err := parseSource(strings.TrimSpace(ts.Source))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		if ts.Name != "" {
			if err := checkName(ts.Name); err != nil {
				return nil, fmt.Errorf("%s (%s): %w", where, ts.Source, err)
			}
			s.Name = ts.Name
		}
		s.Note = ts.Note
		if prev, ok := names[s.Name]; ok {
			return nil, fmt.Errorf("%s (%s): the name %q is taken by %s; give one of them a name", where, ts.Source, s.Name, prev)
		}
		names[s.Name] = ts.Source
		l.Sites = append(l.Sites, s)
		if len(l.Sites) > maxSites {
			return nil, fmt.Errorf("over %d sites", maxSites)
		}
	}
	if len(l.Sites) == 0 {
		return nil, errors.New("the list names no sites; add a [[site]] table with a source")
	}
	return l, nil
}

// EncodeTOML writes sites as a list file, one [[site]] table each.
func EncodeTOML(title string, sites []Site) []byte {
	var b bytes.Buffer
	b.WriteString("# Websites on IPFS to archive. Each run looks every site up again and\n# fetches what it serves now. See https://github.com/jbenet/ipfs2fil#site-lists\n")
	if title != "" {
		fmt.Fprintf(&b, "title = %s\n", Quote(title))
	}
	for _, s := range sites {
		fmt.Fprintf(&b, "\n[[site]]\nsource = %s\n", Quote(s.Source))
		if s.Name != defaultName(s) {
			fmt.Fprintf(&b, "name = %s\n", Quote(s.Name))
		}
		if s.Note != "" {
			fmt.Fprintf(&b, "note = %s\n", Quote(s.Note))
		}
	}
	return b.Bytes()
}

// defaultName is the name a site gets when the list gives it none.
func defaultName(s Site) string {
	d, err := parseSource(s.Source)
	if err != nil {
		return ""
	}
	return d.Name
}

// Quote is s as a TOML basic string.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
