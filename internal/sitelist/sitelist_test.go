package sitelist

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ipfs/boxo/ipns"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"
)

func testCID(s string) cid.Cid {
	h, _ := multihash.Sum([]byte(s), multihash.SHA2_256, -1)
	return cid.NewCidV1(cid.DagProtobuf, h)
}

func TestParse(t *testing.T) {
	c := testCID("x")
	list := `# my sites
benet.ai
Docs.IPFS.tech.   name=ipfs-docs   # a comment
https://fil.pub/
vitalik.eth
ipns://k51qzi5uqu5dlvj2baxnqndepeb86cbk3ng7n3i46uzyxzyqj2xjonzllnv0v8   name=blog
/ipns/example.org
/ipfs/` + c.String() + `/	name=launch-2019
`
	sites, err := Parse(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, kind, host string }{
		{"benet.ai", KindDNSLink, "benet.ai"},
		{"ipfs-docs", KindDNSLink, "docs.ipfs.tech"},
		{"fil.pub", KindURL, "fil.pub"},
		{"vitalik.eth", KindENS, "vitalik.eth"},
		{"blog", KindIPNS, "k51qzi5uqu5dlvj2baxnqndepeb86cbk3ng7n3i46uzyxzyqj2xjonzllnv0v8"},
		{"example.org", KindDNSLink, "example.org"},
		{"launch-2019", KindIPFS, c.String()},
	}
	if len(sites) != len(want) {
		t.Fatalf("%d sites: %+v", len(sites), sites)
	}
	for i, w := range want {
		if s := sites[i]; s.Name != w.name || s.Kind != w.kind || s.Host != w.host {
			t.Errorf("site %d = %+v, want %+v", i, s, w)
		}
	}
	for _, bad := range []string{
		"", "# nothing\n", "not-a-domain", "https://x.org/some/page", "https://u:p@x.org/", "/ipfs/notacid",
		"/ipfs/" + c.String() + "/sub", "x.org name=../up", "x.org color=red", "x.org\nx.org", "x.org bare",
	} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("Parse(%q) took it", bad)
		}
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	site, other := testCID("site"), testCID("other")

	// An IPNS key and a record it signed, pointing at site.
	sk, _, err := ic.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := peer.IDFromPrivateKey(sk)
	name := ipns.NameFromPeer(pid)
	rec, err := ipns.NewRecord(sk, path.FromCid(site), 1, time.Now().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	recBytes, _ := ipns.MarshalRecord(rec)
	// The same, for another key: served under name, it must be refused.
	sk2, _, _ := ic.GenerateEd25519Key(rand.Reader)
	forged, _ := ipns.NewRecord(sk2, path.FromCid(other), 1, time.Now().Add(time.Hour), time.Minute)
	forgedBytes, _ := ipns.MarshalRecord(forged)
	serveForged := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ipns/"+name.String() && r.URL.Query().Get("format") == "ipns-record":
			if serveForged {
				w.Write(forgedBytes)
			} else {
				w.Write(recBytes)
			}
		case r.URL.Path == "/ens/vitalik.eth":
			w.Header().Set("X-Ipfs-Path", "/ipns/"+name.String()+"/")
		case r.URL.Path == "/ens/deep.eth":
			w.Header().Set("X-Ipfs-Path", "/ipfs/"+site.String()+"/index.html")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res := &Resolver{
		HTTP:       srv.Client(),
		Gateways:   []string{srv.URL},
		ENSGateway: srv.URL + "/ens/%s",
		LookupTXT: func(_ context.Context, n string) ([]string, error) {
			switch n {
			case "_dnslink.a.org":
				return []string{"v=spf1 -all", "dnslink=/ipns/b.org"}, nil
			case "_dnslink.b.org":
				return []string{"dnslink=/ipfs/" + other.String()}, nil
			case "_dnslink.loop.org":
				return []string{"dnslink=/ipns/loop.org"}, nil
			case "_dnslink.key.org":
				return []string{"dnslink=/ipns/" + name.String()}, nil
			}
			return nil, errors.New("no such host")
		},
	}
	sites, err := Parse(strings.NewReader("a.org\nkey.org\nvitalik.eth\n/ipns/" + name.String() + "\n/ipfs/" + site.String() + "\nloop.org\ndeep.eth\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		root cid.Cid
		how  string
	}{
		{other, "dnslink, then dnslink"},
		{site, "dnslink, then ipns"},
		{site, "ens via " + strings.TrimPrefix(srv.URL, "http://") + ", then ipns"},
		{site, "ipns"},
		{site, "fixed"},
	}
	for i, w := range want {
		got, err := res.Resolve(ctx, sites[i])
		if err != nil || !got.Root.Equals(w.root) || got.How != w.how {
			t.Errorf("%s: %v %q, %v; want %v %q", sites[i].Source, got.Root, got.How, err, w.root, w.how)
		}
	}
	if _, err := res.Resolve(ctx, sites[5]); err == nil || !strings.Contains(err.Error(), "steps") {
		t.Errorf("a DNSLink loop: %v", err)
	}
	if _, err := res.Resolve(ctx, sites[6]); err == nil || !strings.Contains(err.Error(), "inside a DAG") {
		t.Errorf("a path inside a DAG: %v", err)
	}
	serveForged = true
	if got, err := res.Resolve(ctx, sites[3]); err == nil {
		t.Errorf("a record signed by another key was taken: %v", got.Root)
	}
}
