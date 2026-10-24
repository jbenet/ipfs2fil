// Package manifest defines index/manifest.json, the record of what an archive
// holds and where. It is the contract between the archive builder (which
// writes Roots and Pieces), the store (which writes Copies), and the explorer
// (which reads all of it). Change it with care: old manifests must stay readable.
package manifest

import (
	"encoding/json"
	"os"
	"time"
)

// Format is bumped on any change that old readers cannot ignore.
const Format = 1

type Manifest struct {
	Format int    `json:"format"`
	Tool   string `json:"tool"` // e.g. "ipfs2fil v0.1.0"

	// Root is the archive root: a UnixFS directory linking data/, index/ and
	// explorer/. DataRoot is data/ alone, which depends only on repo content.
	Root     string `json:"root"`
	DataRoot string `json:"dataRoot"`

	Source    Source     `json:"source"`
	Roots     []Root     `json:"roots"`
	Pieces    []Piece    `json:"pieces"`
	Copies    []Copy     `json:"copies,omitempty"`
	Snapshots []Snapshot `json:"snapshots"`
}

// Source describes the repo the archive came from. It never records the
// repo's path or peer identity.
type Source struct {
	RepoVersion int   `json:"repoVersion"`
	Blocks      int64 `json:"blocks"`
	Bytes       int64 `json:"bytes"`
}

// Root kinds, matching the directories under data/.
const (
	KindPinRecursive = "pin-recursive"
	KindPinDirect    = "pin-direct"
	KindMFS          = "mfs"
	KindOrphan       = "orphan"
)

type Root struct {
	CID    string `json:"cid"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"` // pin name, or a guess such as an index page title
	Path   string `json:"path"`           // where it sits under data/, e.g. "pins/recursive/<cid>"
	Bytes  int64  `json:"bytes"`
	Blocks int64  `json:"blocks"`
	// FirstPiece and LastPiece bound the pieces holding this root's blocks.
	// Blocks are laid out depth first, so a root's pieces are contiguous.
	FirstPiece int `json:"firstPiece"`
	LastPiece  int `json:"lastPiece"`
}

type Piece struct {
	Index    int    `json:"index"`
	PieceCID string `json:"pieceCid"` // v2 PieceCID
	CARBytes int64  `json:"carBytes"`
	Blocks   int64  `json:"blocks"`
	// BlockList is the path under index/ listing this piece's multihashes in order.
	BlockList string `json:"blockList"`
	// Snapshot is the index of the snapshot that first stored this piece.
	Snapshot int `json:"snapshot"`
}

// Copy is one provider's data set holding the archive's pieces.
type Copy struct {
	Provider  string         `json:"provider"` // provider id or address
	DataSetID string         `json:"dataSetId"`
	PieceIDs  map[int]string `json:"pieceIds"` // piece index -> on-chain piece id
	Service   string         `json:"service"`  // e.g. "foc-fwss"
}

type Snapshot struct {
	Time      time.Time `json:"time"`
	Root      string    `json:"root"`
	NewPieces []int     `json:"newPieces"`
}

func Read(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(b, &m)
}

func (m *Manifest) Write(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
