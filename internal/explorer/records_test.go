package explorer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

func cborCID(t *testing.T, s string) string {
	t.Helper()
	h, err := multihash.Sum([]byte(s), multihash.SHA2_256, -1)
	if err != nil {
		t.Fatal(err)
	}
	return cid.NewCidV1(cid.DagCBOR, h).String()
}

func TestRecordKind(t *testing.T) {
	post, rec := cborCID(t, "post"), cborCID(t, "like")
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Name: "3k.json", Path: "pins/recursive/x/@a/app.bsky.feed.post/3k.json", CID: post, Kind: "code"}, "post"},
		{Entry{Name: "3l.json", Path: "pins/recursive/x/@a/app.bsky.feed.like/3l.json", CID: rec, Kind: "code"}, "record"},
		{Entry{Name: "root.dag-cbor.json", Path: "pins/recursive/root.dag-cbor.json", CID: rec, Kind: "other"}, "record"},
		{Entry{Name: "3k.json", Path: "a/app.bsky.feed.post/3k.json", CID: post, Kind: "post"}, ""},                                         // already
		{Entry{Name: "pkg.json", Path: "a/pkg.json", CID: "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", Kind: "code"}, ""}, // a raw file
		{Entry{Name: "photo.jpg", Path: "a/photo.jpg", CID: rec, Kind: "image"}, ""},
	}
	for _, c := range cases {
		if got := recordKind(c.e); got != c.want {
			t.Errorf("recordKind(%s) = %q, want %q", c.e.Path, got, c.want)
		}
	}
	es := []Entry{cases[0].e, cases[1].e, cases[4].e}
	types := map[string]tally{"code": {3, 300}}
	if n := fixRecordKinds(es, types); n != 2 {
		t.Fatalf("fixRecordKinds changed %d, want 2", n)
	}
	if es[0].Kind != "post" || es[1].Kind != "record" || es[2].Kind != "code" {
		t.Errorf("kinds = %s %s %s", es[0].Kind, es[1].Kind, es[2].Kind)
	}
	if types["code"][0] != 1 || types["post"][0] != 1 || types["record"][0] != 1 {
		t.Errorf("types = %v", types)
	}
}

func TestSubRepos(t *testing.T) {
	m := &manifest.Manifest{DataRoot: "bafydata"}
	f := func(p, kind string) Entry {
		return Entry{Name: filepath.Base(p), Path: p, CID: "bafy" + strings.NewReplacer("/", "", ".", "").Replace(p), Kind: kind, Bytes: 1, Piece: -1}
	}
	es := []Entry{
		f("pins/recursive/src/proj/.git/HEAD", "text"),
		f("pins/recursive/src/proj/.git/objects/ab", "other"),
		f("pins/recursive/src/proj/.git/refs/heads", "other"),
		f("pins/recursive/src/proj/main.go", "code"),
		f("pins/recursive/src/bare.git/HEAD", "text"),
		f("pins/recursive/src/bare.git/objects/x", "other"),
		f("pins/recursive/src/bare.git/refs/x", "other"),
		f("pins/recursive/old/.ipfs/datastore_spec", "code"),
		f("pins/recursive/old/.ipfs/blocks/x.data", "other"),
		f("pins/recursive/dumps/export.car", "archive"),
		f("pins/recursive/notes/blocks/readme.txt", "text"), // not a repo
	}
	files, err := buildIndex(m, es)
	if err != nil {
		t.Fatal(err)
	}
	var mt meta
	if err := json.Unmarshal(jsValue(files["meta.js"], "meta"), &mt); err != nil {
		t.Fatal(err)
	}
	want := []string{"pins/recursive/dumps/export.car", "pins/recursive/old/.ipfs", "pins/recursive/src/bare.git", "pins/recursive/src/proj"}
	if !reflect.DeepEqual(mt.SubRepos, want) {
		t.Errorf("subRepos = %q, want %q", mt.SubRepos, want)
	}
}

// An archive packed before the post and record kinds lists a Bluesky
// account's records as code; explore serves its index with them as posts
// and records, and leaves the files on disk alone.
func TestRefreshIndex(t *testing.T) {
	dir := t.TempDir()
	m := &manifest.Manifest{DataRoot: "bafydata", Roots: []manifest.Root{{CID: "bafyacct", Path: "pins/recursive/acct", FirstPiece: -1, LastPiece: -1}}}
	post, like := cborCID(t, "post"), cborCID(t, "like")
	es := []Entry{
		{Name: "acct", Path: "pins/recursive/acct", CID: "bafyacct", Kind: "folder", Piece: -1},
		{Name: "3k.json", Path: "pins/recursive/acct/app.bsky.feed.post/3k.json", CID: post, Kind: "code", Bytes: 10, Piece: -1},
		{Name: "3l.json", Path: "pins/recursive/acct/app.bsky.feed.like/3l.json", CID: like, Kind: "code", Bytes: 5, Piece: -1},
	}
	if err := Write(filepath.Join(dir, "explorer"), m, es); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(archiveFiles(dir))
	defer srv.Close()
	get := func(p string) []byte {
		t.Helper()
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
		return b
	}
	var mt meta
	if err := json.Unmarshal(jsValue(get("/explorer/idx/meta.js"), "meta"), &mt); err != nil {
		t.Fatal(err)
	}
	if mt.Types["post"] != (tally{1, 10}) || mt.Types["record"] != (tally{1, 5}) || mt.Types["code"][0] != 0 {
		t.Errorf("types = %v", mt.Types)
	}
	shard := string(get("/explorer/idx/s/" + mt.Shards[0] + ".js"))
	if !strings.Contains(shard, `"3k.json","post"`) || !strings.Contains(shard, `"3l.json","record"`) {
		t.Errorf("shard = %s", shard)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "explorer", "idx", "meta.js")); !strings.Contains(string(b), `"code"`) {
		t.Error("meta.js on disk was rewritten")
	}
}
