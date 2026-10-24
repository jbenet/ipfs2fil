// Package store keeps pieces on Filecoin. The first backend is Filecoin Onchain
// Cloud (PDP storage paid through Filecoin Pay and FWSS); the interface leaves
// room for others.
package store

import (
	"context"
	"io"
)

// DataSet is one provider's set of pieces for one archive. One data set per
// copy; the default is 3 copies on 3 providers.
type DataSet struct {
	Provider string
	ID       string
	Metadata map[string]string // includes withIPFSIndexing so providers announce blocks to IPNI
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
}

type DataSetStatus struct {
	DataSet
	Pieces     int
	LastProven string // as the chain reports it; the store never claims proofs it did not read
}

type Store interface {
	// DataSets finds or creates the archive's data sets, one per copy.
	DataSets(ctx context.Context, archiveRoot string, copies int) ([]DataSet, error)
	// Has reports whether ds already holds a piece with this PieceCID, so re-runs skip it.
	Has(ctx context.Context, ds DataSet, pieceCID string) (bool, error)
	// Upload stores one piece in ds. meta carries ipfsRootCID.
	Upload(ctx context.Context, ds DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (PieceRef, error)
	Status(ctx context.Context, archiveRoot string) (Status, error)
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
