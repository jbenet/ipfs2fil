// Package archive turns a repo into an archive: one UnixFS root linking every
// pin, the MFS tree and orphans under data/, plus index/ and explorer/, packed
// into pieces.
//
// Determinism: data/ and the piece cuts depend only on the repo's blocks and
// roots. Blocks are laid out depth first with roots sorted by CID. A piece is
// cut at the start of a root, or at a block whose multihash ends in CutBits
// zero bits, once the piece holds at least MinPiece bytes; it is always cut
// before MaxPiece. So unchanged subtrees give the same pieces, and a re-run
// skips pieces whose PieceCID is already stored. Anything that varies between
// runs (times, copies) lives in index/.
package archive

import (
	"context"
	"errors"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/repo"
)

type Options struct {
	MinPiece int64 // bytes of CAR data before a content-defined cut is allowed
	MaxPiece int64 // hard cap on CAR bytes; must fit the store's piece limit
	CutBits  int   // a block whose multihash ends in this many zero bits may start a piece
}

// DefaultOptions fit synapse-go's 1 GiB*127/128 piece limit.
func DefaultOptions() Options {
	return Options{MinPiece: 512 << 20, MaxPiece: (1 << 30) * 127 / 128, CutBits: 10}
}

// Piece is a planned piece: a CAR file on disk and its PieceCID.
type Piece struct {
	manifest.Piece
	Path string // the CAR file
}

type Result struct {
	Manifest *manifest.Manifest
	Pieces   []Piece
}

var ErrNotImplemented = errors.New("not implemented yet")

// Build reads r, links its roots, cuts pieces into outDir and returns the
// manifest with Roots and Pieces filled in. Copies are left to the store.
func Build(ctx context.Context, r repo.Repo, outDir string, opts Options) (*Result, error) {
	return nil, ErrNotImplemented
}
