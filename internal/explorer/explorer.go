// Package explorer writes the static HTML explorer that ships inside every
// archive under explorer/. The pages work from any gateway with no server:
// they read index/manifest.json and the name index, and search runs in the
// browser. The designs are in docs/boards.
package explorer

import (
	"errors"

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

var ErrNotImplemented = errors.New("not implemented yet")

// Write renders the explorer for m and entries into dir.
func Write(dir string, m *manifest.Manifest, entries []Entry) error {
	return ErrNotImplemented
}
