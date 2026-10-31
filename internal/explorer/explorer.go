// Package explorer writes the static HTML explorer that ships inside every
// archive under explorer/. The pages work from any gateway with no server, and
// from file:// too: they read the manifest and a sharded name index that are
// written as small JavaScript files (see index.go), and search, filters and
// sort run in the browser. The designs are in docs/boards.
package explorer

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jbenet/ipfs2fil/internal/atproto"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Entry is one name in the search and browse index.
type Entry struct {
	Name  string
	Path  string // under data/
	CID   string
	Kind  string // folder, image, video, audio, doc, page, text, code, archive, disk, other
	Bytes int64
	// Dup is the part of Bytes whose blocks an earlier entry holds too, so
	// totals over many entries count each block once.
	Dup   int64
	Piece int
}

// Kinds lists every Entry.Kind. Sniffed kinds are stored as an index into
// it, so new kinds go at the end: "post" is a social post's record (a
// Bluesky post), "record" any other IPLD record (dag-cbor or dag-json), such
// as a Bluesky like or follow, or a CAR's build record.
var Kinds = []string{"folder", "image", "video", "audio", "page", "doc", "text", "code", "archive", "disk", "other", "post", "record"}

//go:embed assets
var assets embed.FS

// Write renders the explorer for m and entries into dir: the pages from
// assets/ and the index under dir/idx, with a Posts page for each Bluesky
// account among the roots. The output depends only on its inputs, so the
// same archive always gets the same explorer.
func Write(dir string, m *manifest.Manifest, entries []Entry, accounts ...*atproto.Summary) error {
	files, err := buildIndex(m, entries, accounts...)
	if err != nil {
		return err
	}
	if err := copyAssets(dir); err != nil {
		return err
	}
	for name, b := range files {
		p := filepath.Join(dir, "idx", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Shards lists the shard files of the explorer Write made in dir
// (idx/s/<name>.js), in the order of the paths they hold, so that blocks laid
// out in this order keep nearby names together.
func Shards(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "idx", "meta.js"))
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSpace(b)
	b = bytes.TrimPrefix(b, []byte(`I2F.put("meta",`))
	b = bytes.TrimSuffix(b, []byte(");"))
	var mt struct {
		Shards []string `json:"shards"`
	}
	if err := json.Unmarshal(b, &mt); err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, "idx", "meta.js"), err)
	}
	out := make([]string, len(mt.Shards))
	for i, n := range mt.Shards {
		out[i] = n + ".js"
	}
	return out, nil
}

// WriteManifest refreshes only the explorer's copy of the manifest, for when
// the manifest changes after packing (push writes Copies) but entries do not.
func WriteManifest(dir string, m *manifest.Manifest) error {
	if _, err := os.Stat(filepath.Join(dir, "idx", "meta.js")); err != nil {
		return fmt.Errorf("no explorer in %s; run ipfs2fil pack first", dir)
	}
	b, err := jsFile("manifest", m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "idx", "manifest.js"), b, 0o644)
}

func copyAssets(dir string) error {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return err
	}
	return fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}
