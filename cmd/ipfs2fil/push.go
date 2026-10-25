package main

import (
	"context"
	"flag"
	"fmt"
	"sync"
	"time"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/foc"
)

func init() {
	register(&command{
		name:    "push",
		summary: "Upload pieces that are not stored yet (3 copies by default)",
		help: `
Stores the archive in --out on Filecoin Onchain Cloud, one data set per copy,
each on a different provider. Pieces a copy already holds are skipped, so
running push again after Ctrl-C, a failure, or a new pack uploads only what is
missing. Progress is saved to manifest.json after every piece.

Before uploading, push shows the cost and any deposit your wallet must make
into Filecoin Pay, and asks to go ahead (--yes skips the question). The wallet
key comes from --key-file, $IPFS2FIL_KEY_FILE or $IPFS2FIL_PRIVATE_KEY; it is
never printed or written anywhere.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			copiesFlag(fs, o)
			yesFlag(fs, o)
			addStoreFlags(fs)
		},
		run: func(ctx context.Context, o *opts, _ []string) error {
			m, pieces, err := loadArchive(o.out)
			if err != nil {
				return err
			}
			return push(ctx, o, m, pieces)
		},
	})
}

func push(ctx context.Context, o *opts, m *manifest.Manifest, pieces []archive.Piece) error {
	key, err := store.ArchiveKey(m)
	if err != nil {
		return err
	}
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()

	if n, err := missing(ctx, s, m, pieces, o.copies); err != nil {
		return err
	} else if n == 0 {
		fmt.Fprintf(o.stdout, "all %d pieces are already stored in %d copies; nothing to upload\n", len(pieces), o.copies)
		return nil
	}
	sizes := map[string]int64{}
	var total int64
	for _, p := range pieces {
		sizes[p.PieceCID] = p.CARBytes
		total += p.CARBytes
	}
	if ok, err := approveCost(ctx, o, s, key, sizes, total); err != nil || !ok {
		if err == nil {
			fmt.Fprintln(o.stderr, "nothing uploaded")
		}
		return err
	}

	// Before the first piece event, Push finds or creates a data set per
	// copy; creating one on chain takes minutes, so say so instead of a
	// frozen bar.
	_, onChain := s.(funder)
	newSets := max(0, o.copies-len(m.Copies))
	onChain = onChain && newSets > 0
	if onChain {
		fmt.Fprintf(o.stderr, "setting up %d new %s with the providers (a few minutes on chain)...\n", newSets, plural(newSets, "data set", "data sets"))
	}
	start := time.Now()
	var bar *progress
	var mu sync.Mutex
	seen := map[[2]int]int64{} // (copy, piece) -> bytes counted
	final := map[[2]int]bool{} // (copy, piece) -> stored or skipped
	var sent, skipped int
	failed := map[int]bool{}
	err = store.Push(ctx, s, m, pieces, o.copies, saveManifest(o.out), func(ev store.PushEvent) {
		mu.Lock()
		defer mu.Unlock()
		if bar == nil {
			if onChain {
				fmt.Fprintf(o.stderr, "data sets ready in %s\n", roundDur(time.Since(start)))
			}
			bar = newProgress(o.stderr, "uploading", total*int64(o.copies), true)
		}
		k := [2]int{ev.Copy, ev.Piece}
		done := ev.Done
		if ev.Skipped {
			done = ev.Total
		}
		if d := done - seen[k]; d > 0 {
			seen[k] = done
			bar.Add(d)
		}
		if final[k] {
			return
		}
		switch {
		case ev.Err != nil:
			failed[ev.Copy] = true
			final[k] = true
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d failed: %v", ev.Copy+1, ev.Provider, ev.Piece, ev.Err))
		case ev.Skipped:
			skipped++
			final[k] = true
		case ev.Final:
			sent++
			final[k] = true
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d stored", ev.Copy+1, ev.Provider, ev.Piece))
		}
	})
	if bar == nil {
		bar = newProgress(o.stderr, "uploading", total*int64(o.copies), true)
	}
	bar.Done()
	fmt.Fprintf(o.stdout, "uploaded %d pieces, %d were already stored", sent, skipped)
	if len(failed) > 0 {
		fmt.Fprintf(o.stdout, ", %d of %d copies stopped early", len(failed), o.copies)
	}
	fmt.Fprintln(o.stdout)
	if err != nil {
		return fmt.Errorf("%w\nWhat landed is saved; run push again to retry the rest", err)
	}
	fmt.Fprintf(o.stdout, "archive  %s, %d copies\n", m.Root, o.copies)
	fmt.Fprintf(o.stdout, "next     ipfs2fil status --out %s   (proofs start within a day)\n", o.out)
	return nil
}

// missing counts the pieces the copies recorded in m do not hold yet, asking
// the store, so a re-run can say there is nothing to do before any prompt.
func missing(ctx context.Context, s store.Store, m *manifest.Manifest, pieces []archive.Piece, copies int) (int, error) {
	n := 0
	if len(m.Copies) < copies {
		n += (copies - len(m.Copies)) * len(pieces)
	}
	for _, c := range m.Copies[:min(len(m.Copies), copies)] {
		ds := store.DataSet{Provider: c.Provider, ID: c.DataSetID}
		for _, p := range pieces {
			ok, err := s.Has(ctx, ds, p.PieceCID)
			if err != nil {
				return 0, fmt.Errorf("checking provider %s for piece %d: %w", c.Provider, p.Index, err)
			}
			if !ok {
				n++
			}
		}
	}
	return n, nil
}

// approveCost shows what the push costs and asks to go ahead. For FOC it also
// says what deposit is missing and makes it once confirmed.
func approveCost(ctx context.Context, o *opts, s store.Store, key string, sizes map[string]int64, total int64) (bool, error) {
	f, ok := s.(funder)
	if !ok {
		c := store.Estimate(total, len(sizes), o.copies)
		fmt.Fprintf(o.stderr, "storing %d pieces (%s) as %d copies costs about %s USDFC/month plus %s USDFC to add them\n",
			len(sizes), humanBytes(total), o.copies, usdfc(c.Monthly), usdfc(c.OneTime))
		return confirm(o, stdin, "Upload?")
	}
	fund, err := f.Plan(ctx, key, o.copies, sizes)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(o.stderr, "storing %d pieces (%s) as %d copies: %s USDFC/month, %s USDFC in one-time fees\n",
		len(sizes), humanBytes(total), o.copies, foc.USDFC(fund.Monthly), foc.USDFC(fund.Fees))
	if fund.Ready() {
		return confirm(o, stdin, "Upload?")
	}
	if fund.Deposit != nil && fund.Deposit.Sign() > 0 {
		fmt.Fprintf(o.stderr, "your wallet holds %s USDFC; this deposits %s USDFC into Filecoin Pay\n",
			foc.USDFC(fund.Wallet), foc.USDFC(fund.Deposit))
	}
	if fund.Approve {
		fmt.Fprintln(o.stderr, "this also lets Filecoin Warm Storage charge your Filecoin Pay account for storage")
	}
	if ok, err := confirm(o, stdin, "Make the deposit and upload?"); err != nil || !ok {
		return ok, err
	}
	fmt.Fprintln(o.stderr, "sending the deposit and waiting for it to land...")
	start := time.Now()
	if err := fund.Apply(ctx); err != nil {
		return false, err
	}
	fmt.Fprintf(o.stderr, "deposit landed in %s\n", roundDur(time.Since(start)))
	return true, nil
}
