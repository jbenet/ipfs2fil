package sitelist

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
)

func TestParseTOML(t *testing.T) {
	c := testCID("x")
	src := `title = "My sites"
sites = [
  "benet.ai",
  "https://fil.pub/",
  { source = "Docs.IPFS.tech.", name = "ipfs-docs" },
]

[[site]]
source = "/ipfs/` + c.String() + `"
name = "launch-2019"
note = "the launch"
`
	l, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if l.Title != "My sites" || len(l.Sites) != 4 {
		t.Fatalf("%+v", l)
	}
	want := []struct{ name, kind, note string }{
		{"benet.ai", KindDNSLink, ""}, {"fil.pub", KindURL, ""}, {"ipfs-docs", KindDNSLink, ""}, {"launch-2019", KindIPFS, "the launch"},
	}
	for i, w := range want {
		if s := l.Sites[i]; s.Name != w.name || s.Kind != w.kind || s.Note != w.note {
			t.Errorf("site %d = %+v, want %+v", i, s, w)
		}
	}
	// What EncodeTOML writes reads back the same.
	back, err := ParseTOML(EncodeTOML(l.Title, l.Sites))
	if err != nil {
		t.Fatal(err)
	}
	for i := range l.Sites {
		if back.Sites[i].Name != l.Sites[i].Name || back.Sites[i].Source != l.Sites[i].Source || back.Sites[i].Note != l.Sites[i].Note {
			t.Errorf("round trip %d: %+v, want %+v", i, back.Sites[i], l.Sites[i])
		}
	}
	for _, bad := range []string{
		``, `sites = []`, `sites = ["not-a-domain"]`, `sites = ["x.org", "x.org"]`, `colour = "red"`,
		`sites = [{ source = "x.org", color = "red" }]`, `sites = [1]`, "[[site]]\nname = \"x\"", "[[site]]\nsource = \"x.org\"\nnam = \"y\"",
		`sites = [{ source = "x.org", name = "../up" }]`, `gateways = ["ftp://x"]`,
	} {
		if _, err := ParseTOML([]byte(bad)); err == nil {
			t.Errorf("ParseTOML(%q) took it", bad)
		}
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"/x/mine.ipfs-sites.toml": "/x/mine.ipfs-sites.lock.toml",
		"/x/ipfs-sites.toml":      "/x/ipfs-sites.lock.toml",
		"/x/mine.sites":           "",
	} {
		if got := LockPath(in); got != want {
			t.Errorf("LockPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]bool{
		"a.ipfs-sites.toml": true, "ipfs-sites.toml": true, "a.sites": true,
		"a.ipfs-sites.lock.toml": false, "ipfs-sites.lock.toml": false, "a.toml": false,
	} {
		if IsListName(in) != want {
			t.Errorf("IsListName(%q) = %v", in, !want)
		}
	}
	if n := ListName("/x/blogs/ipfs-sites.toml"); n != "blogs" {
		t.Errorf("ListName = %q", n)
	}
	if n := ListName("/x/mine.ipfs-sites.toml"); n != "mine" {
		t.Errorf("ListName = %q", n)
	}
}

func TestLoadLegacy(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "mine.sites")
	os.WriteFile(old, []byte("benet.ai\nx.org name=x\n"), 0o644)
	l, err := Load(old)
	if err != nil || !l.Legacy || len(l.Sites) != 2 || l.Title != "mine" {
		t.Fatalf("%+v %v", l, err)
	}
	other := filepath.Join(dir, "list.txt")
	os.WriteFile(other, []byte("benet.ai\n"), 0o644)
	if l, err := Load(other); err != nil || !l.Legacy {
		t.Fatalf("a line-format list named otherwise: %+v %v", l, err)
	}
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()
	v1, v2 := testCID("v1"), testCID("v2")
	dns := map[string]string{"_dnslink.a.org": "dnslink=/ipfs/" + v1.String()}
	res := &Resolver{LookupTXT: func(_ context.Context, n string) ([]string, error) {
		if v, ok := dns[n]; ok {
			return []string{v}, nil
		}
		return nil, errors.New("no such host")
	}, HTTP: failHTTP()}
	l := &List{Path: "/x/mine.ipfs-sites.toml", Sites: []Site{{Name: "a.org", Source: "a.org", Kind: KindDNSLink, Host: "a.org"}}}
	clock := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fetched := map[cid.Cid]bool{}
	opts := Options{Resolver: res, Now: func() time.Time { return clock },
		Fetch: func(_ context.Context, c cid.Cid) error { fetched[c] = true; return nil },
		Have:  func(c cid.Cid) bool { return fetched[c] }}

	lock := Update(ctx, l, nil, opts)
	if e := lock.Site[0]; e.Root != v1.String() || e.How != "dnslink" || !e.Since.Equal(clock) || e.Stale || lock.List != "mine.ipfs-sites.toml" {
		t.Fatalf("first run %+v", e)
	}
	// Written and read back, the lock is the same.
	path := filepath.Join(t.TempDir(), "mine.ipfs-sites.lock.toml")
	if err := lock.Write(path); err != nil {
		t.Fatal(err)
	}
	lock, err := ReadLock(path)
	if err != nil || len(lock.Site) != 1 || lock.Site[0].Root != v1.String() || !lock.Site[0].Since.Equal(clock) {
		t.Fatalf("read back %+v %v", lock, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "[[site]]") || !strings.HasPrefix(string(b), "# What each site in mine.ipfs-sites.toml") {
		t.Errorf("lock file:\n%s", b)
	}

	// The site moves on: the old root goes in earlier.
	clock = clock.Add(24 * time.Hour)
	dns["_dnslink.a.org"] = "dnslink=/ipfs/" + v2.String()
	lock = Update(ctx, l, lock, opts)
	e := lock.Site[0]
	if e.Root != v2.String() || !e.Since.Equal(clock) || len(e.Earlier) != 1 || e.Earlier[0].Root != v1.String() || !e.Earlier[0].Until.Equal(clock) {
		t.Fatalf("second run %+v", e)
	}

	// It cannot be found: the last root stays, marked stale.
	clock = clock.Add(24 * time.Hour)
	delete(dns, "_dnslink.a.org")
	lock = Update(ctx, l, lock, opts)
	if e := lock.Site[0]; e.Root != v2.String() || !e.Stale || e.Error == "" || len(e.Earlier) != 1 || !e.Checked.Equal(clock.Add(-24*time.Hour)) {
		t.Fatalf("stale run %+v", e)
	}
	// ... unless it was never fetched.
	delete(fetched, v2)
	if e := Update(ctx, l, lock, opts).Site[0]; e.Root != "" || e.Error == "" {
		t.Fatalf("not fetched %+v", e)
	}
}

// failHTTP is a client whose every request fails, so no test goes online.
func failHTTP() *http.Client {
	return &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
