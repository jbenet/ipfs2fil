package main

import (
	"context"
	"flag"
	"fmt"
	"sync"

	"github.com/jbenet/ipfs2fil/internal/store"
)

func init() {
	register(&command{
		name:    "verify",
		summary: "Fetch pieces back and check every block",
		help: `
Downloads every piece from every copy recorded in --out/manifest.json and
checks that it hashes to its PieceCID and that every block in it hashes to its
CID. Keeps going past bad pieces and lists them at the end; run push to
re-upload a piece a copy has lost.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			addStoreFlags(fs)
		},
		run: runVerify,
	})
}

func runVerify(ctx context.Context, o *opts, _ []string) error {
	m, _, err := loadArchive(o.out)
	if err != nil {
		return err
	}
	if len(m.Copies) == 0 {
		return fmt.Errorf("no copies recorded in %s; run 'ipfs2fil push' first", manifestPath(o.out))
	}
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()
	var total int64
	for _, p := range m.Pieces {
		total += p.CARBytes
	}
	bar := newProgress(o.stderr, "verifying", total*int64(len(m.Copies)), true)
	var mu sync.Mutex
	seen := map[[2]int]int64{}
	good, bad := 0, 0
	err = store.Verify(ctx, s, m, func(ev store.VerifyEvent) {
		mu.Lock()
		defer mu.Unlock()
		k := [2]int{ev.Copy, ev.Piece}
		done := ev.Done
		if ev.Err != nil {
			done = ev.Total
		}
		if d := done - seen[k]; d > 0 {
			seen[k] = done
			bar.Add(d)
		}
		switch {
		case ev.Err != nil:
			bad++
		case ev.Done == ev.Total:
			good++
		}
	})
	bar.Done()
	fmt.Fprintf(o.stdout, "%d pieces checked across %d copies: %d good, %d bad\n", good+bad, len(m.Copies), good, bad)
	if err != nil {
		return fmt.Errorf("%w\nRun 'ipfs2fil push --out %s' to re-upload what a copy lost", err, o.out)
	}
	return nil
}
