// Package store keeps pieces on Filecoin. The first backend is Filecoin Onchain
// Cloud (PDP storage paid through Filecoin Pay and FWSS); the interface leaves
// room for others.
package store

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// DataSet is one provider's set of pieces for one archive. One data set per
// copy; the default is 3 copies on 3 providers.
type DataSet struct {
	Provider string // provider id
	ID       string
	Metadata map[string]string // includes withIPFSIndexing so providers announce blocks to IPNI
	// URL is where the provider serves this data set's pieces, if known.
	URL string
}

type PieceRef struct {
	DataSetID string
	PieceID   string
	PieceCID  string
}

type Status struct {
	DataSets []DataSetStatus
	// Funded is how long the current deposit pays for, at today's rate.
	FundedDays int
	// Reserve is the deposit the account's data sets hold back as their
	// lifecycle reserve, as a USDFC amount such as "3"; it is not counted in
	// FundedDays and comes back when the data sets end. ReserveDataSets is
	// how many live data sets the account has, across all archives. Reserve
	// is empty if the store cannot say.
	Reserve         string
	ReserveDataSets int
	// Unused lists data sets with the archive's key that it never uses: a
	// second one on a provider DataSets already holds. They appear when two
	// machines create the first data sets at once, hold no pieces, and keep
	// their reserve until ended.
	Unused []DataSet
}

type DataSetStatus struct {
	DataSet
	Pieces     int
	LastProven string // as the chain reports it; the store never claims proofs it did not read
}

// Store methods take an archive key: the root of the archive's first snapshot,
// manifest.Snapshots[0].Root, or the collection's key (ArchiveKey). The archive root itself changes with every
// snapshot, but the key does not, so re-runs find the same data sets.
type Store interface {
	// DataSets finds or creates the archive's data sets, one per copy, each on
	// a different provider. Existing data sets come first, in a stable order.
	DataSets(ctx context.Context, archive string, copies int) ([]DataSet, error)
	// Has reports whether ds already holds a piece with this PieceCID, so re-runs skip it.
	Has(ctx context.Context, ds DataSet, pieceCID string) (bool, error)
	// Upload stores one piece in ds. meta carries ipfsRootCID. If ds already
	// holds the piece, Upload returns its ref without reading car.
	Upload(ctx context.Context, ds DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (PieceRef, error)
	// Stage sends a piece to ds's provider, which holds it ready to add to
	// ds. Nothing is on chain until Commit; FOC providers drop a staged piece
	// that is not added within about two hours. If the provider already holds
	// the piece, Stage returns without reading car.
	Stage(ctx context.Context, ds DataSet, pieceCID string, car io.Reader, size int64) error
	// Pull stages a piece by having ds's provider fetch it from the provider
	// of from, which holds it staged or added, so the bytes leave this
	// machine once for all copies.
	Pull(ctx context.Context, ds, from DataSet, pieceCID string) error
	// Commit adds staged pieces to ds in one on-chain step and returns their
	// piece ids, in order. Pieces ds already holds keep their ids. If the
	// provider dropped some staged pieces, Commit adds nothing and returns a
	// *GoneError naming them: stage them again, then commit again.
	Commit(ctx context.Context, ds DataSet, pieceCIDs []string, meta map[string]string) ([]string, error)
	// Fetch reads a piece back from ds's provider. The reader returns an error
	// at EOF if the bytes do not hash to pieceCID.
	Fetch(ctx context.Context, ds DataSet, pieceCID string) (io.ReadCloser, error)
	// Status reads the archive's data sets, proofs and funding from chain.
	Status(ctx context.Context, archive string) (Status, error)
}

// GoneError says which staged pieces a provider no longer holds, so they
// cannot be added until they are staged again.
type GoneError struct {
	Provider  string
	PieceCIDs []string
}

func (e *GoneError) Error() string {
	return fmt.Sprintf("provider %s dropped %d staged pieces before they were added: %s",
		e.Provider, len(e.PieceCIDs), strings.Join(e.PieceCIDs, ", "))
}

// Lister is a Store that can read what an archive's data sets hold without
// creating any, which reading the catalog from chain needs.
type Lister interface {
	// Pieces returns the data sets DataSets would use for copies copies, as
	// many as exist, and each one's pieces: PieceCID -> piece id, where piece
	// ids grow in the order pieces were added.
	Pieces(ctx context.Context, archive string, copies int) ([]DataSet, []map[string]string, error)
}

// Cost is a USDFC estimate.
type Cost struct {
	Monthly float64
	OneTime float64
}

// Estimate prices bytes stored as copies data sets of pieces pieces each, using
// FOC list prices: 2.5 USDFC per TiB per month per copy, a 0.12 per month floor
// per data set, and about 0.011 per piece added (0.003 fee plus gas).
func Estimate(bytes int64, pieces, copies int) Cost {
	tib := float64(bytes) / (1 << 40)
	per := 2.5 * tib
	if per < 0.12 {
		per = 0.12
	}
	return Cost{Monthly: per * float64(copies), OneTime: 0.011 * float64(pieces*copies)}
}
