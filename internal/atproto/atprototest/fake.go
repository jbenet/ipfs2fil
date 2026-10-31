// Package atprototest serves a made-up Bluesky account the way a PDS and the
// PLC directory would, for tests.
package atprototest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto"
	"github.com/jbenet/ipfs2fil/internal/car"
)

// Account is a small repo: a profile, posts (one with an image, one replying
// to another), a like, and a blob no record links any more.
type Account struct {
	DID, Handle string
	Commit      cid.Cid
	Image, Old  cid.Cid // the linked and the unlinked blob
	Blocks      map[cid.Cid][]byte
	Server      *httptest.Server
	mu          sync.Mutex
	requests    map[string]int
}

// Requests says how many calls of an XRPC method the PDS answered.
func (a *Account) Requests(method string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.requests[method]
}

func link(c cid.Cid) cbor.Tag { return cbor.Tag{Number: 42, Content: append([]byte{0}, c.Bytes()...)} }

var enc, _ = cbor.CanonicalEncOptions().EncMode()

func (a *Account) put(t testing.TB, codec uint64, v any) cid.Cid {
	t.Helper()
	var b []byte
	if raw, ok := v.([]byte); ok && codec == cid.Raw {
		b = raw
	} else {
		var err error
		if b, err = enc.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := multihash.Sum(b, multihash.SHA2_256, -1)
	c := cid.NewCidV1(codec, h)
	a.Blocks[c] = b
	return c
}

// New builds the account and starts its PDS and PLC directory, pointing the
// atproto package's resolvers at them until the test ends.
func New(t testing.TB) *Account {
	a := &Account{DID: "did:plc:testaliceaaaaaaaaaaaaaaa", Handle: "alice.test", Blocks: map[cid.Cid][]byte{}, requests: map[string]int{}}
	a.Image = a.put(t, cid.Raw, []byte("\x89PNG not really an image"))
	a.Old = a.put(t, cid.Raw, []byte("a blob whose post was deleted"))
	blob := func(c cid.Cid, mime string, size int) map[string]any {
		return map[string]any{"$type": "blob", "ref": link(c), "mimeType": mime, "size": size}
	}
	recs := map[string]cid.Cid{}
	recs["app.bsky.actor.profile/self"] = a.put(t, cid.DagCBOR, map[string]any{
		"$type": "app.bsky.actor.profile", "displayName": "Alice", "description": "testing", "avatar": blob(a.Image, "image/png", 24)})
	first := a.put(t, cid.DagCBOR, map[string]any{"$type": "app.bsky.feed.post", "text": "héllo world", "createdAt": "2024-01-01T00:00:00.000Z", "langs": []any{"en"},
		"facets": []any{map[string]any{"index": map[string]any{"byteStart": 7, "byteEnd": 12},
			"features": []any{map[string]any{"$type": "app.bsky.richtext.facet#link", "uri": "https://example.com/"}}}}})
	recs["app.bsky.feed.post/3aaaaaaaaaaaa"] = first
	recs["app.bsky.feed.post/3bbbbbbbbbbbb"] = a.put(t, cid.DagCBOR, map[string]any{
		"$type": "app.bsky.feed.post", "text": "a picture", "createdAt": "2024-02-01T00:00:00.000Z",
		"embed": map[string]any{"$type": "app.bsky.embed.images", "images": []any{map[string]any{"alt": "a cat", "image": blob(a.Image, "image/png", 24)}}}})
	uri := "at://" + a.DID + "/app.bsky.feed.post/3aaaaaaaaaaaa"
	ref := map[string]any{"uri": uri, "cid": first.String()}
	recs["app.bsky.feed.post/3cccccccccccc"] = a.put(t, cid.DagCBOR, map[string]any{
		"$type": "app.bsky.feed.post", "text": "replying to myself", "createdAt": "2024-03-01T00:00:00.000Z",
		"reply": map[string]any{"root": ref, "parent": ref}})
	recs["app.bsky.feed.like/3dddddddddddd"] = a.put(t, cid.DagCBOR, map[string]any{"$type": "app.bsky.feed.like", "subject": ref, "createdAt": "2024-03-02T00:00:00.000Z"})
	recs["com.example.note/1"] = a.put(t, cid.DagCBOR, map[string]any{"$type": "com.example.note", "body": "not a Bluesky record"})

	// One tree node holds every key, prefix-compressed as the spec says.
	keys := make([]string, 0, len(recs))
	for k := range recs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var es []any
	prev := ""
	for _, k := range keys {
		p := 0
		for p < len(prev) && p < len(k) && prev[p] == k[p] {
			p++
		}
		es = append(es, map[string]any{"p": p, "k": []byte(k[p:]), "v": link(recs[k]), "t": nil})
		prev = k
	}
	data := a.put(t, cid.DagCBOR, map[string]any{"l": nil, "e": es})
	a.Commit = a.put(t, cid.DagCBOR, map[string]any{"did": a.DID, "version": 3, "data": link(data), "rev": "3zzzzzzzzzzzz", "prev": nil, "sig": []byte("signature")})

	a.Server = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.Server.Close)
	old := []any{atproto.PLCDirectory, atproto.HandleResolver, atproto.LookupTXT, atproto.WellKnown}
	atproto.PLCDirectory = a.Server.URL + "/plc"
	atproto.HandleResolver = a.Server.URL
	atproto.LookupTXT = func(_ context.Context, name string) ([]string, error) {
		if name == "_atproto."+a.Handle {
			return []string{"did=" + a.DID}, nil
		}
		return nil, fmt.Errorf("no such host")
	}
	atproto.WellKnown = func(host, path string) string { return a.Server.URL + "/wk/" + host + "/" + path }
	t.Cleanup(func() {
		atproto.PLCDirectory = old[0].(string)
		atproto.HandleResolver = old[1].(string)
		atproto.LookupTXT = old[2].(func(context.Context, string) ([]string, error))
		atproto.WellKnown = old[3].(func(string, string) string)
	})
	return a
}

// SetBlock changes what the PDS serves for c; nil drops it.
func (a *Account) SetBlock(c cid.Cid, b []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b == nil {
		delete(a.Blocks, c)
	} else {
		a.Blocks[c] = b
	}
}

// CAR is the repo as getRepo serves it: the commit, the tree and the records.
func (a *Account) CAR() []byte {
	var buf bytes.Buffer
	w, _ := car.NewWriter(&buf, a.Commit) // one root never fails
	var cs []cid.Cid
	for c := range a.Blocks {
		if c.Type() == cid.DagCBOR {
			cs = append(cs, c)
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].KeyString() < cs[j].KeyString() })
	for _, c := range cs {
		w.Put(c, a.Blocks[c])
	}
	w.Finish()
	return buf.Bytes()
}

func (a *Account) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path := r.URL.Path
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := strings.CutPrefix(path, "/xrpc/"); ok {
		a.requests[m]++
	}
	switch {
	case path == "/plc/"+a.DID:
		json.NewEncoder(w).Encode(map[string]any{
			"id": a.DID, "alsoKnownAs": []string{"at://" + a.Handle},
			"verificationMethod": []any{},
			"service":            []any{map[string]string{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": a.Server.URL}},
		})
	case path == "/plc/"+a.DID+"/log/audit":
		w.Write([]byte(`[{"did":"` + a.DID + `","operation":{"type":"plc_operation"}}]`))
	case path == "/xrpc/com.atproto.sync.getRepo" && q.Get("did") == a.DID:
		w.Header().Set("Content-Type", "application/vnd.ipld.car")
		w.Write(a.CAR())
	case path == "/xrpc/com.atproto.sync.listBlobs" && q.Get("did") == a.DID:
		// Two pages, to follow the cursor.
		if q.Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"cids": []string{a.Image.String()}, "cursor": a.Image.String()})
		} else if q.Get("cursor") == a.Image.String() {
			json.NewEncoder(w).Encode(map[string]any{"cids": []string{a.Old.String()}, "cursor": a.Old.String()})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"cids": []string{}})
		}
	case path == "/xrpc/com.atproto.sync.getBlob" && q.Get("did") == a.DID:
		c, err := cid.Decode(q.Get("cid"))
		if b, ok := a.Blocks[c]; err == nil && ok {
			w.Write(b)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"BlobNotFound","message":"Blob not found"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"NotFound","message":"` + path + `"}`))
	}
}
