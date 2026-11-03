package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/sitelist"
)

func raw(s string) (cid.Cid, []byte) {
	h, _ := multihash.Sum([]byte(s), multihash.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, h), []byte(s)
}

// gateway serves blocks as one-block CARs, as a trustless gateway would.
func gateway(t *testing.T, blocks map[cid.Cid][]byte) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := cid.Decode(strings.TrimPrefix(r.URL.Path, "/ipfs/"))
		b, ok := blocks[c]
		if err != nil || !ok {
			http.NotFound(w, r)
			return
		}
		var buf bytes.Buffer
		cw, _ := car.NewWriter(&buf, c)
		cw.Put(c, b)
		cw.Finish() // too small for a piece; the bytes are written all the same
		w.Write(buf.Bytes())
	}))
	t.Cleanup(s.Close)
	return s
}

func runIn(t *testing.T, dir string, args ...string) (int, string, string, error) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	var out, errb bytes.Buffer
	code, err := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String(), err
}

func TestTool(t *testing.T) {
	a, ab := raw("<h1>site a</h1>")
	b, bb := raw("<h1>site b</h1>")
	gw := gateway(t, map[cid.Cid][]byte{a: ab, b: bb})
	dir, cache := t.TempDir(), t.TempDir()

	// add makes a list, then adds to it.
	if _, _, _, err := runIn(t, dir, "add", "/ipfs/"+a.String(), "home", "-note", "the home page"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runIn(t, dir, "add", "/ipfs/"+b.String(), "other"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := runIn(t, dir, "add", "/ipfs/"+b.String(), "other"); err == nil {
		t.Error("added a name twice")
	}
	l, err := sitelist.Load(filepath.Join(dir, sitelist.Bare))
	if err != nil || len(l.Sites) != 2 || l.Sites[0].Note != "the home page" {
		t.Fatalf("list %+v %v", l, err)
	}

	// car fetches both and writes one CAR with both roots.
	code, out, errs, err := runIn(t, dir, "car", "--cache", cache, "--gateways", gw.URL, "-o", "all.car")
	if err != nil || code != 0 {
		t.Fatalf("car: %d %v\n%s", code, err, errs)
	}
	if !strings.Contains(out, "home") || !strings.Contains(out, a.String()) {
		t.Errorf("car printed %q", out)
	}
	fh, err := os.Open(filepath.Join(dir, "all.car"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	r, err := car.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if roots := r.Roots; len(roots) != 2 || !roots[0].Equals(a) || !roots[1].Equals(b) {
		t.Errorf("roots %v", roots)
	}
	lock, err := sitelist.ReadLock(filepath.Join(dir, sitelist.BareLock))
	if err != nil || len(lock.Site) != 2 || lock.Site[0].Root != a.String() || lock.Site[0].Since == nil {
		t.Fatalf("lock %+v %v", lock, err)
	}

	// check: nothing changed.
	if code, out, _, err := runIn(t, dir, "check"); err != nil || code != 0 || !strings.Contains(out, "No site changed") {
		t.Errorf("check: %d %v %q", code, err, out)
	}
	// show prints the lock.
	if _, out, _, err := runIn(t, dir, "show"); err != nil || !strings.Contains(out, "other") {
		t.Errorf("show: %v %q", err, out)
	}
}

func TestConvert(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "mine.sites"), []byte("benet.ai\nhttps://fil.pub/  name=filpub\n"), 0o644)
	if _, _, _, err := runIn(t, dir, "convert", "mine.sites"); err != nil {
		t.Fatal(err)
	}
	l, err := sitelist.Load(filepath.Join(dir, "mine.ipfs-sites.toml"))
	if err != nil || len(l.Sites) != 2 || l.Sites[1].Name != "filpub" || l.Sites[0].Name != "benet.ai" {
		t.Fatalf("%+v %v", l, err)
	}
	if _, _, _, err := runIn(t, dir, "convert", "mine.sites"); err == nil {
		t.Error("convert replaced a list")
	}
	// Two lists in a folder: name one.
	os.WriteFile(filepath.Join(dir, "other.ipfs-sites.toml"), []byte("[[site]]\nsource = \"x.org\"\n"), 0o644)
	if _, _, _, err := runIn(t, dir, "show"); err == nil || !strings.Contains(err.Error(), "name one") {
		t.Errorf("two lists: %v", err)
	}
}
