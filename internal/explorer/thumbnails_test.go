package explorer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mdag "github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/boxo/ipld/unixfs"
	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/car"
)

// serveBlocks writes blocks as an archive's only piece, serves the archive
// and returns a request helper.
func serveBlocks(t *testing.T, blocks map[cid.Cid][]byte) func(method, path string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // the thumbnail cache
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	m, entries := sample()
	if err := Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var root cid.Cid
	for c := range blocks {
		root = c
		break
	}
	w, err := car.NewWriter(&buf, root)
	if err != nil {
		t.Fatal(err)
	}
	for c, b := range blocks {
		if err := w.Put(c, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "pieces"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "pieces", "piece.car"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	urls := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, dir, nil, "127.0.0.1:0", func(u string) { urls <- u }) }()
	var base string
	select {
	case base = <-urls:
	case err := <-done:
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-done })
	base = strings.TrimSuffix(base, "/explorer/")
	return func(method, path string, hdr map[string]string, body string) (*http.Response, string) {
		t.Helper()
		u := base + path
		if h := hdr["host"]; h != "" { // the same server under another local name
			u = strings.Replace(u, "127.0.0.1", h, 1)
		}
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		for k, v := range hdr {
			if k != "host" {
				req.Header.Set(k, v)
			}
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
}

func raw(t *testing.T, b []byte) *mdag.RawNode {
	n, err := mdag.NewRawNodeWPrefix(b, cid.Prefix{Version: 1, Codec: cid.Raw, MhType: 0x12, MhLength: -1})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func pngOf(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// TestServePath: a folder's files are served by path, so a page's relative
// links work; pages run sandboxed except under localhost, the pages' own
// origin.
func TestServePath(t *testing.T) {
	page, css := raw(t, []byte("<!doctype html><link rel=stylesheet href=s/style.css>")), raw(t, []byte("body{}"))
	sub := unixfs.EmptyDirNode()
	sub.AddNodeLink("style.css", css)
	site := unixfs.EmptyDirNode()
	site.AddNodeLink("index.html", page)
	site.AddNodeLink("s", sub)
	do := serveBlocks(t, map[cid.Cid][]byte{site.Cid(): site.RawData(), sub.Cid(): sub.RawData(), page.Cid(): page.RawData(), css.Cid(): css.RawData()})
	p := "/_i2f/path/" + site.Cid().String()
	r, got := do("GET", p+"/s/style.css", nil, "")
	if r.StatusCode != 200 || got != "body{}" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") {
		t.Errorf("style.css: %s %q %s", r.Status, got, r.Header.Get("Content-Type"))
	}
	r, got = do("GET", p+"/", nil, "")
	if r.StatusCode != 200 || !strings.Contains(got, "stylesheet") || r.Header.Get("Content-Security-Policy") != "sandbox allow-scripts allow-forms allow-popups allow-modals allow-pointer-lock" {
		t.Errorf("index under 127.0.0.1: %s %v", r.Status, r.Header)
	}
	if r, _ = do("GET", p+"/index.html", map[string]string{"host": "localhost"}, ""); r.StatusCode != 200 || r.Header.Get("Content-Security-Policy") != "" {
		t.Errorf("index under localhost: %s %v", r.Status, r.Header)
	}
	if r, _ = do("GET", p+"/nope.html", nil, ""); r.StatusCode != 404 {
		t.Errorf("a missing file: %s", r.Status)
	}
	// content by CID stays sandboxed with no scripts
	if r, _ = do("GET", "/_i2f/content/"+page.Cid().String()+"?name=index.html", map[string]string{"host": "localhost"}, ""); r.Header.Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("content: %v", r.Header)
	}
}

// TestPartialFile: a file whose root is here but not the rest is refused
// before any of it is sent, and listed by _i2f/parts.
func TestPartialFile(t *testing.T) {
	a, b := raw(t, bytes.Repeat([]byte("a"), 3000)), raw(t, bytes.Repeat([]byte("b"), 3000))
	fsn := unixfs.NewFSNode(unixfs.TFile)
	file := mdag.NodeWithData(nil)
	for _, l := range []*mdag.RawNode{a, b} {
		fsn.AddBlockSize(3000)
		file.AddNodeLink("", l)
	}
	d, _ := fsn.GetBytes()
	file.SetData(d)
	whole := raw(t, []byte("whole"))
	do := serveBlocks(t, map[cid.Cid][]byte{file.Cid(): file.RawData(), a.Cid(): a.RawData(), whole.Cid(): whole.RawData()}) // not b
	r, got := do("GET", "/_i2f/content/"+file.Cid().String()+"?name=v.mp4", nil, "")
	if r.StatusCode != 404 || !strings.Contains(got, "Only 2 of the 3 parts") {
		t.Errorf("partial file: %s %q", r.Status, got)
	}
	_, got = do("POST", "/_i2f/parts", nil, file.Cid().String()+"\n"+whole.Cid().String()+"\n")
	var parts map[string][2]int
	json.Unmarshal([]byte(got), &parts)
	if len(parts) != 1 || parts[file.Cid().String()] != [2]int{2, 3} {
		t.Errorf("parts: %s", got)
	}
}

// TestThumbnailers: Office files' own previews, 404 for files with none,
// and thumbnails the browser sends kept only from the explorer's pages.
func TestThumbnailers(t *testing.T) {
	var zb bytes.Buffer
	z := zip.NewWriter(&zb)
	f, _ := z.Create("[Content_Types].xml")
	f.Write([]byte("<Types/>"))
	f, _ = z.Create("docProps/thumbnail.png")
	f.Write(pngOf(200, 100))
	z.Close()
	doc, text := raw(t, zb.Bytes()), raw(t, []byte("just words"))
	video := raw(t, []byte("\x00\x00\x00\x18ftypmp42not really a video"))
	do := serveBlocks(t, map[cid.Cid][]byte{doc.Cid(): doc.RawData(), text.Cid(): text.RawData(), video.Cid(): video.RawData()})
	r, got := do("GET", "/_i2f/content/"+doc.Cid().String()+"?name=a.docx&w=100", nil, "")
	if img, _, err := image.Decode(strings.NewReader(got)); r.StatusCode != 200 || err != nil || img.Bounds().Dx() != 100 {
		t.Errorf("docx preview: %s %v", r.Status, err)
	}
	if r, _ = do("GET", "/_i2f/content/"+text.Cid().String()+"?name=a.txt&w=100", nil, ""); r.StatusCode != 404 {
		t.Errorf("text thumbnail: %s", r.Status)
	}
	path := "/_i2f/thumb/" + video.Cid().String() + "?w=100"
	frame := string(pngOf(320, 180))
	for name, hdr := range map[string]map[string]string{
		"no header":     {"Referer": "http://127.0.0.1/explorer/browse.html"},
		"from a page":   {"X-I2F-Thumb": "1", "Referer": "http://127.0.0.1/_i2f/path/x/index.html"},
		"from no pages": {"X-I2F-Thumb": "1"},
	} {
		if r, _ := do("POST", path, hdr, frame); r.StatusCode != 400 {
			t.Errorf("%s: kept a thumbnail: %s", name, r.Status)
		}
	}
	if _, err := os.Stat(filepath.Join(newThumbCache().dir)); err == nil {
		// only .none files so far, from the text file
		es, _ := os.ReadDir(newThumbCache().dir)
		for _, e := range es {
			if strings.HasPrefix(e.Name(), video.Cid().String()) {
				t.Errorf("cached %s before the explorer sent one", e.Name())
			}
		}
	}
	hdr := map[string]string{"X-I2F-Thumb": "1"}
	// the Referer has to name this server, so build it from a request
	r0, _ := do("GET", "/_i2f/info", nil, "")
	hdr["Referer"] = "http://" + r0.Request.URL.Host + "/explorer/browse.html"
	if r, got := do("POST", path, hdr, frame); r.StatusCode != 204 {
		t.Fatalf("explorer's thumbnail: %s %s", r.Status, got)
	}
	r, got = do("GET", "/_i2f/content/"+video.Cid().String()+"?name=v.mp4&w=100", nil, "")
	if img, _, err := image.Decode(strings.NewReader(got)); r.StatusCode != 200 || err != nil || img.Bounds().Dx() != 100 {
		t.Errorf("kept thumbnail: %s %v", r.Status, err)
	}
}
