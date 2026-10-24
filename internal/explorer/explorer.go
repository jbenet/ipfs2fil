// Package explorer writes the static HTML explorer that ships inside every
// archive under explorer/. The pages work from any gateway with no server, and
// from file:// too: they read the manifest and a sharded name index that are
// written as small JavaScript files (see index.go), and search, filters and
// sort run in the browser. The designs are in docs/boards.
package explorer

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// Entry is one name in the search and browse index.
type Entry struct {
	Name  string
	Path  string // under data/
	CID   string
	Kind  string // folder, image, video, audio, doc, page, text, code, archive, disk, other
	Bytes int64
	Piece int
}

// Kinds lists every Entry.Kind, in the order the pages show them.
var Kinds = []string{"folder", "image", "video", "audio", "page", "doc", "text", "code", "archive", "disk", "other"}

//go:embed assets
var assets embed.FS

// Write renders the explorer for m and entries into dir: the pages from
// assets/ and the index under dir/idx. The output depends only on its inputs,
// so the same archive always gets the same explorer.
func Write(dir string, m *manifest.Manifest, entries []Entry) error {
	files, err := buildIndex(m, entries)
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
