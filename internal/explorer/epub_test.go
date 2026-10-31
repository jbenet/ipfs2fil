package explorer

import (
	"archive/zip"
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestEPUBCover: explore finds a book's cover the ways EPUB 3 and EPUB 2
// name it, and makes a thumbnail of it.
func TestEPUBCover(t *testing.T) {
	var pic bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 60))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	png.Encode(&pic, img)
	book := func(manifest, meta string) []byte {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		add := func(name string, data []byte) {
			w, _ := z.Create(name)
			w.Write(data)
		}
		add("mimetype", []byte("application/epub+zip"))
		add("META-INF/container.xml", []byte(`<?xml version="1.0"?><container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/package.opf"/></rootfiles></container>`))
		add("OPS/package.opf", []byte(`<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf"><metadata>`+meta+`</metadata><manifest>`+manifest+`</manifest></package>`))
		add("OPS/images/the cover.png", pic.Bytes())
		add("OPS/images/other.png", []byte("not this one"))
		z.Close()
		return b.Bytes()
	}
	for _, c := range []struct {
		name, manifest, meta string
		want                 bool
	}{
		{"epub3", `<item id="o" href="images/other.png" media-type="image/png"/><item id="c" href="images/the%20cover.png" media-type="image/png" properties="cover-image"/>`, ``, true},
		{"epub2", `<item id="cover-pic" href="images/other.png" media-type="image/png"/><item id="x" href="images/the%20cover.png" media-type="image/png"/>`, `<meta name="cover" content="x"/>`, true},
		{"by name", `<item id="o" href="images/other.png" media-type="image/png"/><item id="i" href="images/the%20cover.png" media-type="image/png"/>`, ``, true},
		{"none", `<item id="o" href="images/other.png" media-type="image/png"/>`, ``, false},
	} {
		got := epubCover(bytes.NewReader(book(c.manifest, c.meta)))
		if c.want != bytes.Equal(got, pic.Bytes()) {
			t.Errorf("%s: got %d bytes, want the cover: %v", c.name, len(got), c.want)
		}
	}
	if epubCover(bytes.NewReader([]byte("not a zip"))) != nil {
		t.Error("a file that is not a zip has a cover")
	}
	th := newThumbs().thumbnail(bytes.NewReader(epubCover(bytes.NewReader(book(`<item id="c" href="images/the%20cover.png" media-type="image/png" properties="cover-image"/>`, "")))), 30)
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(th)); err != nil || cfg.Width != 20 || cfg.Height != 30 {
		t.Errorf("thumbnail %v %v", cfg, err)
	}
}
