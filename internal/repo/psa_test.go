package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
)

// psaServer lists pins as a Pinning Services API endpoint would, one per
// page, newest first.
func psaServer(t *testing.T, token string, pins []map[string]any) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"reason":"UNAUTHORIZED","details":"bad token"}}`))
			return
		}
		if r.URL.Path != "/psa/pins" || r.URL.Query().Get("status") != "pinned" {
			http.NotFound(w, r)
			return
		}
		before := r.URL.Query().Get("before")
		var res []any
		for _, p := range pins { // newest first
			if before == "" || p["created"].(string) < before {
				res = append(res, p)
				break
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"count": len(pins), "results": res})
	}))
	t.Cleanup(s.Close)
	return s
}

func TestPSA(t *testing.T) {
	d := testDAG{}
	a, b := d.put(t, cid.Raw, []byte("leaf a")), d.put(t, cid.Raw, []byte("leaf b"))
	root := d.node(t, "root", a, d.node(t, "mid", b))
	partial := d.node(t, "partial", a, b)
	gw := d.gateway(t, partial, b)
	oldGW, oldMax := Gateways, maxOpenCARs
	Gateways, maxOpenCARs = []string{gw.URL}, 1
	t.Cleanup(func() { Gateways, maxOpenCARs = oldGW, oldMax })
	PSADir = t.TempDir()
	pin := func(id, created string, c cid.Cid, name string) map[string]any {
		return map[string]any{"requestid": id, "status": "pinned", "created": created,
			"pin": map[string]any{"cid": c.String(), "name": name, "meta": map[string]string{"app": "test"}}}
	}
	srv := psaServer(t, "sekret", []map[string]any{
		pin("r3", "2024-03-01T00:00:00Z", partial, "broken"),
		pin("r2", "2024-02-01T00:00:00Z", a, ""),
		pin("r1", "2024-01-01T00:00:00Z", root, "my site"),
	})
	src := "psa+" + srv.URL + "/psa"
	if !IsPSA(src) || !IsPSA("psa:pinata") || IsPSA("psa:nope") || IsPSA("psa+https://u:p@host/psa") || IsPSA("psa+https://host/psa?token=x") {
		t.Fatal("IsPSA")
	}
	if p, err := Discover(src); err != nil || p != src {
		t.Fatalf("Discover = %q, %v", p, err)
	}

	t.Setenv("IPFS2FIL_PSA_TOKEN", "wrong")
	r, _ := Open(src)
	if _, err := r.Pins(context.Background()); err == nil || !strings.Contains(err.Error(), "UNAUTHORIZED") {
		t.Fatalf("a wrong token: %v", err)
	}

	t.Setenv("IPFS2FIL_PSA_TOKEN", "sekret")
	r, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Kind() != KindPSA {
		t.Fatal(r.Kind())
	}
	ctx := context.Background()
	pins, err := r.Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pins {
		names = append(names, p.Root.String()+" "+p.Mode.String()+" "+p.Name)
	}
	if len(pins) != 3 || !pins[0].Root.Equals(root) || pins[0].Name != "my site" || pins[1].Name != a.String() || pins[2].Name != PSAFile {
		t.Fatalf("pins:\n%s", strings.Join(names, "\n"))
	}
	data, err := r.Get(ctx, pins[2].Root.Hash())
	if err != nil {
		t.Fatal(err)
	}
	var doc PSADoc
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Pins) != 3 {
		t.Fatalf("psa.json %v: %s", err, data)
	}
	if p := doc.Pins[2]; p.RequestID != "r3" || !strings.Contains(p.Missing, "blocks missing") {
		t.Errorf("the broken pin: %+v", p)
	}
	if p := doc.Pins[0]; p.Meta["app"] != "test" || p.Missing != "" || p.Created != "2024-01-01T00:00:00Z" {
		t.Errorf("the first pin: %+v", p)
	}
	got := map[string]bool{}
	err = r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
		b, err := r.Get(ctx, h)
		if err != nil || len(b) != size || got[string(h)] {
			t.Errorf("block %s: %v (listed twice: %v)", h.B58String(), err, got[string(h)])
		}
		got[string(h)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for c, want := range d {
		if c.Equals(partial) {
			continue
		}
		if b, err := r.Get(ctx, c.Hash()); err != nil || !bytes.Equal(b, want) {
			t.Errorf("%s: %v", c, err)
		}
	}
	if len(got) != 5 { // a, b, root, mid, psa.json
		t.Errorf("%d blocks", len(got))
	}
}
