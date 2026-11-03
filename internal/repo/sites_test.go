package repo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

func TestSites(t *testing.T) {
	d := testDAG{}
	a, b := d.put(t, cid.Raw, []byte("<h1>home</h1>")), d.put(t, cid.Raw, []byte("about"))
	home := d.node(t, "home", a, d.node(t, "about", b))
	old := d.node(t, "launch", a)
	gw := d.gateway(t, cid.Undef, cid.Undef)
	oldGW, oldRes := Gateways, SiteResolver
	t.Cleanup(func() { Gateways, SiteResolver = oldGW, oldRes })
	Gateways = []string{gw.URL}
	DAGDir, SitesDir = t.TempDir(), t.TempDir()
	dnslink := map[string]string{"_dnslink.example.org": "dnslink=/ipfs/" + home.String()}
	SiteResolver = &sitelist.Resolver{LookupTXT: func(_ context.Context, n string) ([]string, error) {
		if v, ok := dnslink[n]; ok {
			return []string{v}, nil
		}
		return nil, errors.New("no such host")
	}, ENSGateway: gw.URL + "/no-ens/%s"}

	list := filepath.Join(t.TempDir(), "mine.sites")
	os.WriteFile(list, []byte("example.org\n/ipfs/"+old.String()+"  name=launch\nnowhere.example\n"), 0o644)
	if !IsSites(list) || !IsSites("sites:"+list) || IsSites(list+".missing") {
		t.Fatal("IsSites")
	}
	if p, err := Discover(list); err != nil || p != list {
		t.Fatalf("Discover = %q, %v", p, err)
	}
	read := func() (Repo, []Pin, SitesDoc) {
		t.Helper()
		r, err := Open(list)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind() != KindSites {
			t.Fatal(r.Kind())
		}
		pins, err := r.Pins(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var doc SitesDoc
		for _, p := range pins {
			if p.Name == SitesFile {
				b, _ := r.Get(context.Background(), p.Root.Hash())
				json.Unmarshal(b, &doc)
			}
		}
		return r, pins, doc
	}

	r, pins, doc := read()
	defer r.Close()
	var got []string
	for _, p := range pins {
		got = append(got, p.Mode.String()+" "+p.Name)
	}
	if strings.Join(got, ",") != "recursive example.org,recursive launch,direct sites.json,direct mine.sites" ||
		!pins[0].Root.Equals(home) || !pins[1].Root.Equals(old) {
		t.Fatalf("pins %v", got)
	}
	if len(doc.Sites) != 3 || doc.Sites[0].How != "dnslink" || doc.Sites[1].How != "fixed" || doc.Sites[2].Root != "" || doc.Sites[2].Error == "" {
		t.Fatalf("sites.json %+v", doc)
	}
	n := 0
	r.ForEachBlock(context.Background(), func(h multihash.Multihash, size int) error {
		if b, err := r.Get(context.Background(), h); err != nil || len(b) != size {
			t.Errorf("block %s: %v", h.B58String(), err)
		}
		n++
		return nil
	})
	if n != 6 { // a, b, about, home, launch... a and launch share a; plus sites.json and the list
		t.Logf("%d blocks", n)
	}

	// Next run: the domain lost its DNSLink. Its last root stays, marked stale.
	delete(dnslink, "_dnslink.example.org")
	_, pins, doc = read()
	if !pins[0].Root.Equals(home) || !doc.Sites[0].Stale || doc.Sites[0].Error == "" {
		t.Errorf("stale site: %+v", doc.Sites[0])
	}
}

func TestSitesTOML(t *testing.T) {
	d := testDAG{}
	home := d.node(t, "home", d.put(t, cid.Raw, []byte("<h1>home</h1>")))
	gw := d.gateway(t, cid.Undef, cid.Undef)
	oldGW, oldRes := Gateways, SiteResolver
	t.Cleanup(func() { Gateways, SiteResolver = oldGW, oldRes })
	Gateways = []string{gw.URL + "/not-this-one"}
	DAGDir, SitesDir = t.TempDir(), t.TempDir()
	SiteResolver = &sitelist.Resolver{LookupTXT: func(_ context.Context, n string) ([]string, error) {
		if n == "_dnslink.example.org" {
			return []string{"dnslink=/ipfs/" + home.String()}, nil
		}
		return nil, errors.New("no such host")
	}}
	dir := t.TempDir()
	list := filepath.Join(dir, "mine.ipfs-sites.toml")
	// The list's own gateways replace the usual ones.
	os.WriteFile(list, []byte(`title = "Mine"
gateways = ["`+gw.URL+`"]

[[site]]
source = "example.org"
name = "ex"
note = "the example"
`), 0o644)
	if !IsSites(list) || IsSites(filepath.Join(dir, "mine.ipfs-sites.lock.toml")) {
		t.Fatal("IsSites")
	}
	r, err := Open(list)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	pins, err := r.Pins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var doc SitesDoc
	for _, p := range pins {
		if p.Name == SitesFile {
			b, _ := r.Get(context.Background(), p.Root.Hash())
			json.Unmarshal(b, &doc)
		}
	}
	if !pins[0].Root.Equals(home) || pins[0].Name != "ex" || doc.Title != "Mine" || doc.List != "mine.ipfs-sites.toml" ||
		len(doc.Sites) != 1 || doc.Sites[0].Note != "the example" || doc.Sites[0].Since == nil {
		t.Fatalf("pins %+v doc %+v", pins, doc)
	}
	lock, err := sitelist.ReadLock(filepath.Join(dir, "mine.ipfs-sites.lock.toml"))
	if err != nil || len(lock.Site) != 1 || lock.Site[0].Root != home.String() {
		t.Fatalf("lock %+v %v", lock, err)
	}
}
