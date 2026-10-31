package explorer

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math/rand"
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

// TestServeContent: explore serves files from the CARs in pieces/, typed
// from their first bytes, and this build's pages over the packed ones.
func TestServeContent(t *testing.T) {
	dir := t.TempDir()
	m, entries := sample()
	if err := Write(filepath.Join(dir, "explorer"), m, entries); err != nil {
		t.Fatal(err)
	}
	// an old copy of a page on disk, which explore should not serve
	if err := os.WriteFile(filepath.Join(dir, "explorer", "explorer.js"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a JPEG in two raw leaves under a dag-pb file node, and a text file
	jpeg := append([]byte("\xff\xd8\xff\xe0"), make([]byte, 5000)...)
	rand.New(rand.NewSource(1)).Read(jpeg[4:])
	var blocks [][2]any
	fsn := unixfs.NewFSNode(unixfs.TFile)
	file := mdag.NodeWithData(nil)
	for _, part := range [][]byte{jpeg[:3000], jpeg[3000:]} {
		leaf, err := mdag.NewRawNodeWPrefix(part, cid.Prefix{Version: 1, Codec: cid.Raw, MhType: 0x12, MhLength: -1})
		if err != nil {
			t.Fatal(err)
		}
		blocks = append(blocks, [2]any{leaf.Cid(), leaf.RawData()})
		fsn.AddBlockSize(uint64(len(part)))
		if err := file.AddNodeLink("", leaf); err != nil {
			t.Fatal(err)
		}
	}
	b, err := fsn.GetBytes()
	if err != nil {
		t.Fatal(err)
	}
	file.SetData(b)
	blocks = append(blocks, [2]any{file.Cid(), file.RawData()})
	text, err := mdag.NewRawNodeWPrefix([]byte("hello, archive\n"), cid.Prefix{Version: 1, Codec: cid.Raw, MhType: 0x12, MhLength: -1})
	if err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, [2]any{text.Cid(), text.RawData()})
	// a Bluesky record: dag-cbor {"$type": "note", "text": "hi"}
	recData := []byte{0xa2, 0x65, '$', 't', 'y', 'p', 'e', 0x64, 'n', 'o', 't', 'e', 0x64, 't', 'e', 'x', 't', 0x62, 'h', 'i'}
	recCID, err := cid.Prefix{Version: 1, Codec: cid.DagCBOR, MhType: 0x12, MhLength: -1}.Sum(recData)
	if err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, [2]any{recCID, recData})
	// a dag-json record, linking the dag-cbor one
	djData := []byte(`{"build":{"/":"` + recCID.String() + `"},"version":"1.4.0"}`)
	djCID, err := cid.Prefix{Version: 1, Codec: cid.DagJSON, MhType: 0x12, MhLength: -1}.Sum(djData)
	if err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, [2]any{djCID, djData})
	var buf bytes.Buffer
	w, err := car.NewWriter(&buf, file.Cid())
	if err != nil {
		t.Fatal(err)
	}
	for _, bl := range blocks {
		if err := w.Put(bl[0].(cid.Cid), bl[1].([]byte)); err != nil {
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
	base = strings.TrimSuffix(base, "/explorer/")
	do := func(method, path, rng, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
	r, got := do("GET", "/_i2f/content/"+file.Cid().String(), "", "")
	if r.StatusCode != 200 || got != string(jpeg) || r.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("content: %s, %d bytes, %s", r.Status, len(got), r.Header.Get("Content-Type"))
	}
	if r, got = do("GET", "/_i2f/content/"+file.Cid().String(), "bytes=2990-3009", ""); r.StatusCode != 206 || got != string(jpeg[2990:3010]) {
		t.Errorf("range across leaves: %s %q", r.Status, got)
	}
	if r, _ = do("GET", "/_i2f/content/"+text.Cid().String()+"?name=a.html&dl=1", "", ""); r.Header.Get("Content-Security-Policy") != "sandbox" ||
		!strings.Contains(r.Header.Get("Content-Disposition"), "a.html") {
		t.Errorf("a page is not sandboxed or not a download: %v", r.Header)
	}
	if r, got = do("GET", "/_i2f/content/"+recCID.String()+"?name=1.json", "", ""); r.StatusCode != 200 || got != `{"$type":"note","text":"hi"}` ||
		r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("a record: %s %q %s", r.Status, got, r.Header.Get("Content-Type"))
	}
	if r, got = do("GET", "/_i2f/content/"+djCID.String()+"?name=r.json", "", ""); r.StatusCode != 200 || got != string(djData) ||
		r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("a dag-json record: %s %q %s", r.Status, got, r.Header.Get("Content-Type"))
	}
	missing, _ := cid.Decode("bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy")
	if r, _ = do("GET", "/_i2f/content/"+missing.String(), "", ""); r.StatusCode != 404 {
		t.Errorf("a missing block: %s", r.Status)
	}
	_, got = do("POST", "/_i2f/kinds", "", file.Cid().String()+"\n"+text.Cid().String()+"\nnot-a-cid\n"+missing.String()+"\n")
	var kinds map[string]string
	json.Unmarshal([]byte(got), &kinds)
	if kinds[file.Cid().String()] != "image" || kinds[text.Cid().String()] != "text" || len(kinds) != 2 {
		t.Errorf("kinds: %s", got)
	}
	if _, got = do("GET", "/explorer/explorer.js", "", ""); got == "old" {
		t.Error("explore served the packed explorer.js, not this build's")
	}
	if r, _ = do("GET", "/_i2f/info", "", ""); r.StatusCode != 200 {
		t.Errorf("info: %s", r.Status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestThumbnail(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for x := 0; x < 200; x++ {
		for y := 0; y < 200; y++ {
			src.Set(x, y, color.RGBA{255, 0, 0, 255}) // left half red
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, src); err != nil {
		t.Fatal(err)
	}
	out := newThumbs().thumbnail(bytes.NewReader(b.Bytes()), 100)
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 100 || img.Bounds().Dy() != 50 {
		t.Errorf("thumbnail is %v, want 100x50", img.Bounds())
	}
	if r, g, _, _ := img.At(10, 25).RGBA(); r < 0xe000 || g > 0x2000 {
		t.Errorf("left is not red")
	}
	// EXIF orientation 6: the stored picture turns a quarter clockwise
	if x, y := upright(0, 0, 200, 400, 6); x != 0 || y != 199 {
		t.Errorf("upright(0,0) for orientation 6 = %d,%d", x, y)
	}
	exif := []byte("\xff\xd8\xff\xe1\x00\x22Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00\xff\xda")
	if o := orientation(exif); o != 6 {
		t.Errorf("orientation = %d, want 6", o)
	}
	if newThumbs().thumbnail(strings.NewReader("not an image"), 100) != nil {
		t.Error("made a thumbnail of text")
	}
}
