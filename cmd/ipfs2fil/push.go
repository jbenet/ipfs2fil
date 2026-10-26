package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
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

Each piece leaves this machine once: the copies split the pieces and upload
to their providers at the same time, and each provider copies the pieces it
did not get from the provider that did (if it cannot, the piece is uploaded
to it from here). Sending a piece and adding it on chain are separate steps:
while the network base fee is at or above --max-base-fee, adding waits for it
to fall but sending carries on (up to an hour ahead, as providers drop pieces
not added within about two hours), and the pieces that piled up are added
together.

Before uploading, push shows the cost and any deposit your wallet must make
into Filecoin Pay, and asks to go ahead (--yes skips the question). The wallet
key comes from --key-file, $IPFS2FIL_KEY_FILE or $IPFS2FIL_PRIVATE_KEY; it is
never printed or written anywhere.

With --all, pushes every repo 'ipfs2fil repos add' listed into one shared set
of data sets, in the order added, after one confirmation for all of them.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			copiesFlag(fs, o)
			yesFlag(fs, o)
			addStoreFlags(fs)
			allFlag(fs, o)
		},
		run: func(ctx context.Context, o *opts, _ []string) error {
			if o.all {
				return pushAll(ctx, o)
			}
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
	return upload(ctx, o, s, m, pieces, total)
}

// upload stores the archive's pieces once the cost is approved.
func upload(ctx context.Context, o *opts, s store.Store, m *manifest.Manifest, pieces []archive.Piece, total int64) error {
	if err := checkShared(ctx, s, m, o.copies); err != nil {
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
	seen := map[[2]int]int64{}   // (copy, piece) -> bytes counted
	final := map[[2]int]bool{}   // (copy, piece) -> stored or skipped
	waiting := map[[2]int]bool{} // (copy, piece) -> staged, not yet added on chain
	pulling := map[[2]int]bool{} // (copy, piece) -> its provider is fetching it from another
	var sent, skipped, pulled int
	failed := map[int]bool{}
	defer logTo(nil)
	err := store.Push(ctx, s, m, pieces, o.copies, saveManifest(o.out), func(ev store.PushEvent) {
		mu.Lock()
		defer mu.Unlock()
		if bar == nil {
			if onChain {
				fmt.Fprintf(o.stderr, "data sets ready in %s\n", roundDur(time.Since(start)))
			}
			bar = newProgress(o.stderr, "uploading", total*int64(o.copies), true)
			logTo(bar)
		}
		k := [2]int{ev.Copy, ev.Piece}
		switch {
		case ev.Pulling:
			pulling[k] = true
		case ev.Staged:
			delete(pulling, k)
			waiting[k] = true
		case ev.Final || ev.PullErr != nil:
			delete(pulling, k)
			delete(waiting, k)
		}
		var notes []string
		if n := len(pulling); n > 0 {
			notes = append(notes, fmt.Sprintf("%d %s copying between providers", n, plural(n, "piece", "pieces")))
		}
		if n := len(waiting); n > 0 {
			notes = append(notes, fmt.Sprintf("%d %s sent, waiting to go on chain", n, plural(n, "piece", "pieces")))
		}
		bar.Suffix(strings.Join(notes, " · "))
		done := ev.Done
		if ev.Skipped {
			done = ev.Total
		}
		if d := done - seen[k]; d > 0 {
			seen[k] = done
			if ev.Skipped || ev.Pulled { // these bytes did not leave this machine
				bar.Skip(d)
			} else {
				bar.Add(d)
			}
		}
		if final[k] {
			return
		}
		switch {
		case ev.Dropped:
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d was dropped by the provider while it waited to go on chain; sending it again",
				ev.Copy+1, ev.Provider, ev.Piece))
		case ev.PullErr != nil:
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d could not be copied from provider %s, uploading it from here: %v",
				ev.Copy+1, ev.Provider, ev.Piece, ev.From, ev.PullErr))
		case ev.Final && ev.Pulled && ev.Err == nil:
			sent++
			pulled++
			final[k] = true
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d copied from provider %s, stored", ev.Copy+1, ev.Provider, ev.Piece, ev.From))
		case ev.Err != nil:
			failed[ev.Copy] = true
			final[k] = true
			bar.Log(fmt.Sprintf("copy %d (provider %s): piece %d failed: %v", ev.Copy+1, ev.Provider, ev.Piece, ev.Err))
		case ev.Staged:
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
	bar.Suffix("")
	bar.Done()
	fmt.Fprintf(o.stdout, "stored %d pieces, skipped %d already stored", sent, skipped)
	if pulled > 0 {
		fmt.Fprintf(o.stdout, "; providers copied %d from each other, so each piece left this machine once", pulled)
	}
	if len(failed) > 0 {
		fmt.Fprintf(o.stdout, ", %d of %d copies stopped early", len(failed), o.copies)
	}
	fmt.Fprintln(o.stdout)
	if err != nil {
		return fmt.Errorf("%w\nWhat landed is saved; run push again to retry the rest", err)
	}
	fmt.Fprintf(o.stdout, "archive  %s, %d copies\n", m.Root, o.copies)
	if !o.all {
		fmt.Fprintf(o.stdout, "next     ipfs2fil status --out %s   (proofs start within a day)\n", o.out)
	}
	return nil
}

// checkShared makes sure the pieces of earlier repos that hold this archive's
// shared blocks are stored in every copy, so the archive is whole once pushed.
func checkShared(ctx context.Context, s store.Store, m *manifest.Manifest, copies int) error {
	if m.Shared == nil || len(m.Shared.Pieces) == 0 {
		return nil
	}
	key, err := store.ArchiveKey(m)
	if err != nil {
		return err
	}
	st, err := s.Status(ctx, key)
	if err != nil {
		return err
	}
	need := "push the repos it shares blocks with first ('ipfs2fil push --all' pushes them in order)"
	if len(st.DataSets) < copies {
		return fmt.Errorf("this archive leaves %d blocks to earlier repos' pieces, which are not stored in %d copies yet; %s",
			m.Shared.Blocks, copies, need)
	}
	for _, d := range st.DataSets[:copies] {
		for _, p := range m.Shared.Pieces {
			ok, err := s.Has(ctx, d.DataSet, p.PieceCID)
			if err != nil {
				return fmt.Errorf("checking provider %s for shared piece %s: %w", d.Provider, p.PieceCID, err)
			}
			if !ok {
				return fmt.Errorf("provider %s does not hold piece %s yet, which has %d of this archive's blocks; %s",
					d.Provider, p.PieceCID, p.Blocks, need)
			}
		}
	}
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
	// Fees are synapse-go's ceiling: one add-pieces fee per piece, though
	// push adds pieces in batches. They come out of the deposit.
	fmt.Fprintf(o.stderr, "storing %d pieces (%s) as %d copies: %s USDFC/month, at most %s USDFC in one-time fees (paid from the deposit)\n",
		len(sizes), humanBytes(total), o.copies, foc.USDFC(fund.Monthly), foc.USDFC(fund.Fees))
	if b, ok := s.(balancer); ok {
		// FIL and USDFC both have 18 decimals.
		if bal, err := b.Balances(ctx); err != nil {
			fmt.Fprintf(o.stderr, "could not read the wallet's balances: %v\n", err)
		} else {
			fmt.Fprintf(o.stderr, "wallet holds %s FIL and %s USDFC; Filecoin Pay holds %s USDFC deposited, %s available (not locked up by data sets)\n",
				foc.USDFC(bal.FIL), foc.USDFC(bal.USDFC), foc.USDFC(bal.Deposit), foc.USDFC(bal.Available))
		}
	}
	if fund.Ready() {
		return confirm(o, stdin, "Upload?")
	}
	if fund.Deposit != nil && fund.Deposit.Sign() > 0 {
		fmt.Fprintf(o.stderr, "this deposits %s USDFC from the wallet into Filecoin Pay\n", foc.USDFC(fund.Deposit))
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
