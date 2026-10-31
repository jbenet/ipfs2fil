package explorer

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"net/url"
	"path"
	"strings"
)

// epubCover returns the cover picture of the EPUB in r, or nil when it has
// none that explore can find: the item marked cover-image (EPUB 3), else the
// one an EPUB 2 <meta name="cover"> names, else an image item called cover.
func epubCover(r io.Reader) []byte {
	data, err := io.ReadAll(io.LimitReader(r, maxThumbSource+1))
	if err != nil || len(data) > maxThumbSource {
		return nil
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil
	}
	read := func(name string) []byte {
		f, err := z.Open(name)
		if err != nil {
			return nil
		}
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, maxThumbSource))
		return b
	}
	var container struct {
		Rootfiles []struct {
			Path string `xml:"full-path,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if xml.Unmarshal(read("META-INF/container.xml"), &container) != nil || len(container.Rootfiles) == 0 {
		return nil
	}
	opfPath := container.Rootfiles[0].Path
	var opf struct {
		Meta []struct {
			Name    string `xml:"name,attr"`
			Content string `xml:"content,attr"`
		} `xml:"metadata>meta"`
		Items []struct {
			ID    string `xml:"id,attr"`
			Href  string `xml:"href,attr"`
			Type  string `xml:"media-type,attr"`
			Props string `xml:"properties,attr"`
		} `xml:"manifest>item"`
	}
	if xml.Unmarshal(read(opfPath), &opf) != nil {
		return nil
	}
	named := ""
	for _, m := range opf.Meta {
		if m.Name == "cover" {
			named = m.Content
		}
	}
	href, rank := "", 0
	for _, it := range opf.Items {
		r := 0
		switch {
		case !strings.HasPrefix(it.Type, "image/"):
		case strings.Contains(" "+it.Props+" ", " cover-image "):
			r = 3
		case it.ID == named:
			r = 2
		case strings.Contains(strings.ToLower(it.ID+" "+it.Href), "cover"):
			r = 1
		}
		if r > rank {
			href, rank = it.Href, r
		}
	}
	if href == "" {
		return nil
	}
	// hrefs are URLs relative to the package file
	if u, err := url.Parse(href); err == nil {
		href = u.Path
	}
	return read(path.Join(path.Dir(opfPath), href))
}
