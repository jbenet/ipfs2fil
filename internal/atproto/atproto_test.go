package atproto_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/atproto"
	"github.com/jbenet/ipfs2fil/internal/atproto/atprototest"
)

func TestIsAccount(t *testing.T) {
	for s, want := range map[string]bool{
		"at://alice.test": true, "at://did:plc:abc": true, "did:plc:abc": true, "did:web:example.com": true, "@alice.test": true,
		"/home/x/.ipfs": false, "http://127.0.0.1:5001": false, "@notahandle": false, "export.car": false,
	} {
		if got := atproto.IsAccount(s); got != want {
			t.Errorf("IsAccount(%q) = %v", s, got)
		}
	}
	if got := atproto.Account("at://Alice.Test/app.bsky.feed.post/1"); got != "alice.test" {
		t.Errorf("Account = %q", got)
	}
}

func TestResolve(t *testing.T) {
	a := atprototest.New(t)
	for _, in := range []string{"at://alice.test", "@alice.test", a.DID, "at://" + a.DID} {
		id, err := atproto.Resolve(context.Background(), http.DefaultClient, in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if id.DID != a.DID || id.Handle != "alice.test" || id.PDS != a.Server.URL || id.Name() != "@alice.test" {
			t.Errorf("%s: %+v", in, id)
		}
		if !strings.Contains(string(id.DIDDoc), a.DID) || !strings.Contains(string(id.AuditLog), "plc_operation") {
			t.Errorf("%s: documents %s / %s", in, id.DIDDoc, id.AuditLog)
		}
	}
	if _, err := atproto.Resolve(context.Background(), http.DefaultClient, "at://nobody.test"); err == nil {
		t.Error("resolved a handle no one has")
	}
}

func TestFetch(t *testing.T) {
	a := atprototest.New(t)
	id, err := atproto.Resolve(context.Background(), http.DefaultClient, "at://alice.test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f, err := atproto.Fetch(context.Background(), http.DefaultClient, id, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Blobs != 2 || f.New != 2 || len(f.Missing) != 0 || f.RepoBytes == 0 {
		t.Errorf("fetched %+v", f)
	}
	for _, name := range []string{atproto.RepoFile, atproto.DIDDocFile, atproto.AuditLogFile, atproto.IdentityFile,
		filepath.Join(atproto.BlobDir, a.Image.String()), filepath.Join(atproto.BlobDir, a.Old.String())} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}
	// Again: blobs already here are not fetched twice.
	before := a.Requests("com.atproto.sync.getBlob")
	if f, err = atproto.Fetch(context.Background(), http.DefaultClient, id, dir, nil); err != nil || f.New != 0 {
		t.Fatalf("second fetch %+v %v", f, err)
	}
	if a.Requests("com.atproto.sync.getBlob") != before {
		t.Error("fetched blobs it already had")
	}
	// A blob the server lists but no longer has is skipped, not fatal.
	os.Remove(filepath.Join(dir, atproto.BlobDir, a.Old.String()))
	a.SetBlock(a.Old, nil)
	if f, err = atproto.Fetch(context.Background(), http.DefaultClient, id, dir, nil); err != nil || len(f.Missing) != 1 {
		t.Fatalf("missing blob: %+v %v", f, err)
	}
	// A blob whose bytes do not match its CID is an error.
	a.SetBlock(a.Old, []byte("something else"))
	if _, err = atproto.Fetch(context.Background(), http.DefaultClient, id, dir, nil); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("bad blob: %v", err)
	}
}

func TestSummarize(t *testing.T) {
	a := atprototest.New(t)
	get := func(c cid.Cid) ([]byte, error) {
		if b, ok := a.Blocks[c]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("no block %s", c)
	}
	s, err := atproto.Summarize(get, a.Commit, "@alice.test")
	if err != nil {
		t.Fatal(err)
	}
	if s.DID != a.DID || s.Rev != "3zzzzzzzzzzzz" || s.Commit != a.Commit.String() || s.Profile["displayName"] != "Alice" || s.Profile["avatar"] != a.Image.String() {
		t.Errorf("summary %+v", s)
	}
	want := map[string]int{"app.bsky.actor.profile": 1, "app.bsky.feed.post": 3, "app.bsky.feed.like": 1, "com.example.note": 1}
	if fmt.Sprint(s.Counts) != fmt.Sprint(want) {
		t.Errorf("counts %v", s.Counts)
	}
	if len(s.Posts) != 3 || s.Posts[0].Text != "replying to myself" || s.Posts[2].Text != "héllo world" {
		t.Fatalf("posts %+v", s.Posts)
	}
	if !strings.HasSuffix(s.Posts[0].Reply, "/3aaaaaaaaaaaa") || len(s.Posts[1].Images) != 1 || s.Posts[1].Images[0][0] != a.Image.String() || s.Posts[1].Images[0][1] != "a cat" {
		t.Errorf("reply and image: %+v", s.Posts[:2])
	}
	if len(s.Records) != 1 || len(s.Records["com.example.note"]) != 1 || s.Records["com.example.note"][0][0] != "1" {
		t.Errorf("records %v", s.Records)
	}
	// "héllo " is 7 bytes but 6 UTF-16 units
	if f := s.Posts[2].Facets; len(f) != 1 || fmt.Sprint(f[0]) != "[6 11 link https://example.com/]" {
		t.Errorf("facets %v", f)
	}
	if s.Posts[0].Root != "" || s.Time == "" {
		t.Errorf("root %q, time %q", s.Posts[0].Root, s.Time)
	}
	// Every record and each linked blob, once, named after where it is used.
	files := map[string]string{}
	for _, f := range s.Files {
		files[f.Path] = f.CID.String()
	}
	if len(files) != 7 || files["app.bsky.feed.like/3dddddddddddd.json"] == "" || files["com.example.note/1.json"] == "" ||
		files["blobs/profile avatar.png"] != a.Image.String() {
		t.Errorf("files %v", files)
	}
	if b := s.Blobs; b.Linked != 1 || b.Held != 1 || b.Images != 1 || b.Bytes == 0 {
		t.Errorf("blobs %+v", b)
	}
	if _, err := atproto.Summarize(get, a.Image, ""); err == nil {
		t.Error("a blob summarized as a commit")
	}
}

func TestTIDTime(t *testing.T) {
	// 2023-04-12T17:39:57Z, from a real post's rkey
	tm, ok := atproto.TIDTime("3jt6wgfuuvs23")
	if !ok || tm.UTC().Format("2006-01-02") != "2023-04-12" {
		t.Errorf("TIDTime = %v %v", tm, ok)
	}
	if _, ok := atproto.TIDTime("not a tid"); ok {
		t.Error("read a time from a non-TID")
	}
}

func TestDIDDoc(t *testing.T) {
	did, handle, pds, ok := atproto.DIDDoc([]byte(`{"id":"did:plc:x","alsoKnownAs":["at://a.example"],"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example"}]}`))
	if !ok || did != "did:plc:x" || handle != "a.example" || pds != "https://pds.example" {
		t.Errorf("DIDDoc = %q %q %q %v", did, handle, pds, ok)
	}
	if _, _, _, ok := atproto.DIDDoc([]byte(`[{"did":"did:plc:x"}]`)); ok {
		t.Error("read a PLC log as a DID document")
	}
}

func TestJSON(t *testing.T) {
	a := atprototest.New(t)
	b, err := atproto.JSON(a.Blocks[a.Commit])
	if err != nil || !strings.Contains(string(b), `"did":"`+a.DID+`"`) || !strings.Contains(string(b), `"$link":"`) || !strings.Contains(string(b), `"$bytes":"`) {
		t.Errorf("JSON = %s, %v", b, err)
	}
}
