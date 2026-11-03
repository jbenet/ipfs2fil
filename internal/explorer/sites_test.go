package explorer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

func TestSites(t *testing.T) {
	m, entries := sample()
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	doc, _ := json.Marshal(SiteList{Type: SitesType, Title: "Mine", Sites: []sitelist.Entry{
		{Site: sitelist.Site{Name: "my site", Source: "example.org", Kind: sitelist.KindDNSLink}, Root: "bafysite", How: "dnslink", Since: &since},
		{Site: sitelist.Site{Name: "gone.eth", Source: "gone.eth", Kind: sitelist.KindENS}, Error: "no such name"},
	}})
	l, ok := ParseSites(doc)
	if !ok {
		t.Fatal("ParseSites")
	}
	if _, ok := ParseSites([]byte(`{"$type":"other"}`)); ok {
		t.Error("took another type")
	}
	files, err := buildIndexWith(m, entries, Extras{Sites: []*SiteList{l}})
	if err != nil {
		t.Fatal(err)
	}
	var mt meta
	if err := json.Unmarshal(jsValue(files["meta.js"], "meta"), &mt); err != nil {
		t.Fatal(err)
	}
	if len(mt.Sites) != 2 {
		t.Fatalf("sites %+v", mt.Sites)
	}
	s := mt.Sites[0]
	if s.Path != "pins/recursive/bafysite" || s.Bytes != 5000 || s.Live != "https://example.org/" || s.Since != "2026-11-01T00:00:00Z" || s.List != "Mine" {
		t.Errorf("site %+v", s)
	}
	if g := mt.Sites[1]; g.Path != "" || g.Live != "https://gone.eth.limo/" || g.Error == "" {
		t.Errorf("missing site %+v", g)
	}
}

func TestSiteRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_i2f/site/{cid}/{rest...}", siteRedirect)
	for q, want := range map[string]string{
		"/_i2f/site/QmUNLLsPACCz1vLxQVkXqqLX5R1X345qqfHbsf67hvA3Nn/?where=1": "http://bafybeiczsscdsbs7ffqz55asqdf3smv6klcw3gofszvwlyarci47bgf354.ipfs.localhost:8484/",
		"/_i2f/site/QmUNLLsPACCz1vLxQVkXqqLX5R1X345qqfHbsf67hvA3Nn/a/b.html": "http://bafybeiczsscdsbs7ffqz55asqdf3smv6klcw3gofszvwlyarci47bgf354.ipfs.localhost:8484/a/b.html",
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8484"+q, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		got := w.Body.String()
		if w.Code == http.StatusFound {
			got = w.Header().Get("Location")
		}
		if got != want {
			t.Errorf("%s: %d %q, want %q", q, w.Code, got, want)
		}
	}
	if !pageHost("bafy.ipfs.localhost:8484") || pageHost("evil.example:8484") {
		t.Error("pageHost")
	}
}
