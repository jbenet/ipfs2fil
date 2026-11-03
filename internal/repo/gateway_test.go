package repo

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
)

// testDAG is a little DAG for the gateway: blocks by CID.
type testDAG map[cid.Cid][]byte

func (d testDAG) put(t *testing.T, codec uint64, b []byte) cid.Cid {
	t.Helper()
	h, _ := multihash.Sum(b, multihash.SHA2_256, -1)
	c := cid.NewCidV1(codec, h)
	d[c] = b
	return c
}

// node is a dag-cbor block linking kids.
func (d testDAG) node(t *testing.T, name string, kids ...cid.Cid) cid.Cid {
	t.Helper()
	var ls []any
	for _, k := range kids {
		ls = append(ls, cbor.Tag{Number: 42, Content: append([]byte{0}, k.Bytes()...)})
	}
	b, err := cbor.Marshal(map[string]any{"name": name, "links": ls})
	if err != nil {
		t.Fatal(err)
	}
	return d.put(t, cid.DagCBOR, b)
}

// carOf is the DAG under root as a gateway sends it, leaving out drop.
func (d testDAG) carOf(t *testing.T, root cid.Cid, drop ...cid.Cid) []byte {
	t.Helper()
	var buf bytes.Buffer
	section := func(parts ...[]byte) {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		var vb [binary.MaxVarintLen64]byte
		buf.Write(vb[:binary.PutUvarint(vb[:], uint64(n))])
		for _, p := range parts {
			buf.Write(p)
		}
	}
	section(car.Header(root))
	var walk func(c cid.Cid)
	walk = func(c cid.Cid) {
		for _, x := range drop {
			if x.Equals(c) {
				return
			}
		}
		b, ok := d[c]
		if !ok {
			return
		}
		section(c.Bytes(), b)
		if c.Type() == cid.DagCBOR {
			var n struct{ Links []cbor.Tag }
			cbor.Unmarshal(b, &n)
			for _, l := range n.Links {
				kc, _ := cid.Cast(l.Content.([]byte)[1:])
				walk(kc)
			}
		}
	}
	walk(root)
	return buf.Bytes()
}

// gateway serves d as a trustless gateway would, but sends broken's DAG
// without one of its blocks.
func (d testDAG) gateway(t *testing.T, broken cid.Cid, drop cid.Cid) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := cid.Decode(strings.TrimPrefix(r.URL.Path, "/ipfs/"))
		if _, ok := d[c]; err != nil || !ok || r.URL.Query().Get("format") != "car" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		if c.Equals(broken) {
			w.Write(d.carOf(t, c, drop))
			return
		}
		w.Write(d.carOf(t, c))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestFetchDAG(t *testing.T) {
	d := testDAG{}
	a, b := d.put(t, cid.Raw, []byte("leaf a")), d.put(t, cid.Raw, []byte("leaf b"))
	root := d.node(t, "root", a, d.node(t, "mid", b))
	partial := d.node(t, "partial", a, b)
	gw := d.gateway(t, partial, b)
	old := Gateways
	Gateways = []string{gw.URL + "/nothing-here", gw.URL}
	t.Cleanup(func() { Gateways = old })

	dir := t.TempDir()
	dest := dir + "/root.car"
	if err := fetchDAG(t.Context(), root, dest); err != nil {
		t.Fatal(err)
	}
	r, err := openCAR(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if pins, _ := r.Pins(t.Context()); len(pins) != 1 || !pins[0].Root.Equals(root) {
		t.Errorf("pins %+v", pins)
	}
	for _, c := range []cid.Cid{a, b, root} {
		if got, err := r.Get(t.Context(), c.Hash()); err != nil || !bytes.Equal(got, d[c]) {
			t.Errorf("%s: %v", c, err)
		}
	}
	err = fetchDAG(t.Context(), partial, dir+"/partial.car")
	if err == nil || !strings.Contains(err.Error(), "1 blocks missing") {
		t.Errorf("a partial DAG: %v", err)
	}
}
