package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/repo"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/multiformats/go-multihash"
)

func init() {
	register(&command{
		name:    "scan",
		summary: "Read the repo and say what an archive would hold and cost",
		help: `
Reads every block, pin and the MFS root of the repo and prints what an archive
would hold, about how many pieces it would cut, and what storing it costs in
USDFC at Filecoin Onchain Cloud list prices. Nothing is written anywhere.

Safe to run while the daemon runs. With --json, prints one JSON object.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoFlag(fs, o)
			copiesFlag(fs, o)
			jsonFlag(fs, o)
		},
		run: runScan,
	})
}

type scanResult struct {
	Repo        string  `json:"repo"`
	Kind        string  `json:"kind"`
	RepoVersion int     `json:"repoVersion"`
	Blocks      int64   `json:"blocks"`
	Bytes       int64   `json:"bytes"`
	Pins        int     `json:"pins"`
	Recursive   int     `json:"recursivePins"`
	Direct      int     `json:"directPins"`
	MFSRoot     string  `json:"mfsRoot,omitempty"`
	Pieces      int     `json:"pieces"` // estimate
	Copies      int     `json:"copies"`
	Monthly     float64 `json:"monthlyUSDFC"`
	OneTime     float64 `json:"oneTimeUSDFC"`
	// AtMinimum is set when the archive is small enough that each copy pays
	// the per-data-set floor rather than a per-byte rate.
	AtMinimum bool `json:"atMinimum"`

	describe string // the repo's kind and version, for people
}

func runScan(ctx context.Context, o *opts, _ []string) error {
	s, err := scanRepo(ctx, o)
	if err != nil {
		return err
	}
	if o.json {
		e := json.NewEncoder(o.stdout)
		e.SetIndent("", "  ")
		return e.Encode(s)
	}
	printScan(o, s)
	return nil
}

func scanRepo(ctx context.Context, o *opts) (*scanResult, error) {
	r, err := openRepo(o)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	s := &scanResult{Repo: r.Path(), Kind: r.Kind(), RepoVersion: int(r.Version()), Copies: o.copies}
	s.describe = repo.Describe(r)

	var p *progress
	if !o.json {
		p = newProgress(o.stderr, "reading blocks", 0, false)
	}
	err = r.ForEachBlock(ctx, func(_ multihash.Multihash, size int) error {
		s.Blocks++
		s.Bytes += int64(size)
		if p != nil {
			p.Add(1)
		}
		return ctx.Err()
	})
	if p != nil {
		p.Done()
	}
	if err != nil {
		return nil, err
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading pins: %w", err)
	}
	s.Pins = len(pins)
	for _, pin := range pins {
		if pin.Mode == repo.Recursive {
			s.Recursive++
		} else {
			s.Direct++
		}
	}
	if c, ok, err := r.MFSRoot(ctx); err != nil {
		return nil, fmt.Errorf("reading the MFS root: %w", err)
	} else if ok {
		s.MFSRoot = c.String()
	}
	s.Pieces = estimatePieces(s.Bytes, archive.DefaultOptions())
	c := store.Estimate(s.Bytes, s.Pieces, s.Copies)
	s.Monthly, s.OneTime = c.Monthly, c.OneTime
	s.AtMinimum = c.Monthly <= store.Estimate(0, 0, s.Copies).Monthly
	return s, nil
}

// estimatePieces guesses the piece count before packing: content-defined cuts
// land between MinPiece and MaxPiece, plus one piece for index/ and explorer/.
func estimatePieces(bytes int64, opt archive.Options) int {
	avg := (opt.MinPiece + opt.MaxPiece) / 2
	if avg <= 0 {
		return 1
	}
	return int((bytes+avg-1)/avg) + 1
}

func printScan(o *opts, s *scanResult) {
	w := o.stdout
	fmt.Fprintf(w, "repo     %s (%s)\n", s.Repo, s.describe)
	fmt.Fprintf(w, "blocks   %d, %s\n", s.Blocks, humanBytes(s.Bytes))
	fmt.Fprintf(w, "pins     %d (%d recursive, %d direct)\n", s.Pins, s.Recursive, s.Direct)
	if s.MFSRoot != "" {
		fmt.Fprintf(w, "mfs      %s\n", s.MFSRoot)
	}
	fmt.Fprintf(w, "pieces   about %d\n", s.Pieces)
	fmt.Fprintf(w, "cost     %s USDFC/month for %d copies, plus about %s USDFC to add the pieces\n",
		usdfc(s.Monthly), s.Copies, usdfc(s.OneTime))
	if s.AtMinimum {
		fmt.Fprintf(w, "         (the minimum: each copy costs at least %s USDFC/month, which covers up to about %s)\n",
			usdfc(store.Estimate(0, 0, 1).Monthly), humanBytes(minimumCovers()))
	}
}

func usdfc(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.2f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// minimumCovers is how many bytes one copy stores for the per-data-set floor.
func minimumCovers() int64 {
	floor := store.Estimate(0, 0, 1).Monthly
	perTiB := store.Estimate(1<<40, 0, 1).Monthly
	return int64(floor / perTiB * (1 << 40))
}
