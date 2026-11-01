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

	// Shared, if set, lists the blocks this archive links but leaves to other
	// archives' pieces, because an earlier repo in the same collection already
	// stores them.
	Shared *Shared `json:"shared,omitempty"`
	// Unstored lists the repo's pins (and MFS root) that have no block in
	// the archive, so they are not under data/: identity CIDs, whose content
	// is the CID itself, and roots whose block the repo did not have. An
	// export pins them again. Only CID, Kind and Name are set.
	Unstored []Root `json:"unstored,omitempty"`
	// Collection is the key of the collection the archive belongs to. Every
	// archive in a collection stores into the collection's data sets, so the
	// store keys them by Collection instead of Snapshots[0].Root. It is only
	// in outDir/manifest.json, never inside the archive.
	Collection string `json:"collection,omitempty"`
}

// Shared counts the blocks other archives hold for this one.
type Shared struct {
	Blocks int64         `json:"blocks"`
	Bytes  int64         `json:"bytes"`
	Pieces []SharedPiece `json:"pieces"` // sorted by PieceCID
}

// SharedPiece is another archive's data piece that holds some of this
// archive's blocks.
type SharedPiece struct {
	PieceCID string `json:"pieceCid"`
	Archive  string `json:"archive"` // the other archive's key, its Snapshots[0].Root
	Blocks   int64  `json:"blocks"`  // how many of this archive's blocks it holds
}

// Source describes the repo the archive came from. It never records the
// repo's path or peer identity.
type Source struct {
	// Kind is the sort of repo, as repo.Kind names it; empty for fs-repos,
	// so archives of fs-repos read the same as before kinds existed.
	Kind        string `json:"kind,omitempty"`
	RepoVersion int    `json:"repoVersion"`
	Blocks      int64  `json:"blocks"`
	Bytes       int64  `json:"bytes"`
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
	// Own is the bytes of its blocks that no earlier root holds, so Own adds
	// up across roots where Bytes counts shared blocks once per root. Archives
	// packed before Own was added have none.
	Own int64 `json:"own,omitempty"`
	// FirstPiece and LastPiece bound the pieces holding this root's blocks.
	// Blocks are laid out depth first, so a root's pieces are contiguous.
	FirstPiece int `json:"firstPiece"`
	LastPiece  int `json:"lastPiece"`
}

// The parts of an archive's own blocks. Head pieces hold the root, the
// catalog record and what depends on the manifest (index/manifest.json, the
// explorer's copy of it and its summary), and name the root in their CAR
// headers. The rest depends on the repo alone, so it is cut like data
// pieces and an unchanged part keeps its piece: index pieces hold the block
// lists under index/pieces (a catalog reader needs those), meta pieces the
// data/ folders and the rest of explorer/. Archives packed before parts
// existed have pieces with no part after their data pieces, holding all of it.
//
// A bundle piece holds what changed in one push, for one or more archives:
// for each, its head blocks, any of its index and meta blocks no stored
// piece holds, and its last data piece if that is small (then the bundle
// has a BlockList, listing that archive's data blocks in it). Its CAR
// header names each archive's root and record in turn, and the records are
// its first blocks. Archives that changed in the same push share the piece.
const (
	PartIndex  = "index"
	PartMeta   = "meta"
	PartHead   = "head"
	PartBundle = "bundle"
)

type Piece struct {
	Index    int    `json:"index"`
	PieceCID string `json:"pieceCid"` // v2 PieceCID
	CARBytes int64  `json:"carBytes"`
	Blocks   int64  `json:"blocks"`
	// BlockList is the path under index/ listing this piece's multihashes in order.
	BlockList string `json:"blockList"`
	// Part is "" for a piece of repo blocks, else which of the archive's own
	// blocks it holds (PartIndex, PartMeta, PartHead).
	Part string `json:"part,omitempty"`
	// DataBytes is, for a bundle, the CAR bytes of this archive's data
	// blocks in it (the rest are other blocks, or other archives').
	DataBytes int64 `json:"dataBytes,omitempty"`
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
