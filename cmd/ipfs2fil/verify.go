package main

import (
	"context"
	"flag"
	"fmt"
	"sync"

	"github.com/jbenet/ipfs2fil/internal/ipni"
	"github.com/jbenet/ipfs2fil/internal/store"
)

// checkIPNI asks delegated routing about CIDs. Tests replace it.
var checkIPNI = ipni.Check

func init() {
	register(&command{
		name:    "verify",
		summary: "Fetch pieces back and check every block",
		help: `
Downloads every piece from every copy recorded in --out/manifest.json and
checks that it hashes to its PieceCID and that every block in it hashes to its
CID. Keeps going past bad pieces and lists them at the end; run push to
re-upload a piece a copy has lost. No wallet key needed: --address is enough.

Then asks --ipni, a delegated routing endpoint, whether a sample of the
archive's CIDs (the archive root, data root and some pinned roots) are
announced, which is what makes them findable by CID on the IPFS network.
Providers announce a while after a push, so a new archive may not be
announced yet; this part is reported but never fails verify.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			addReadStoreFlags(fs)
			fs.StringVar(&o.ipni, "ipni", ipni.DefaultEndpoint, "delegated routing endpoint to check announcements with (empty: skip)")
			fs.IntVar(&o.ipniSample, "ipni-sample", 20, "how many CIDs to check with --ipni")
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
		case ev.Final:
			good++
		}
	})
	bar.Done()
	fmt.Fprintf(o.stdout, "%d pieces checked across %d copies: %d good, %d bad\n", good+bad, len(m.Copies), good, bad)
	if o.ipni != "" && o.ipniSample > 0 {
		cids := ipni.Sample(m, o.ipniSample)
		res := checkIPNI(ctx, nil, o.ipni, cids, 8)
		fmt.Fprintf(o.stdout, "%d of %d sampled CIDs announced at %s\n", res.Announced, res.Checked, res.Endpoint)
		for i, miss := range res.Missing {
			if i == 5 {
				fmt.Fprintf(o.stdout, "  and %d more\n", len(res.Missing)-i)
				break
			}
			fmt.Fprintf(o.stdout, "  not announced: %s (%s)\n", miss.CID, miss.Reason)
		}
		if len(res.Missing) > 0 {
			fmt.Fprintln(o.stdout, "providers announce a few hours after a push; if this persists, the CIDs are stored but not findable over IPFS")
		}
	}
	if err != nil {
		return fmt.Errorf("%w\nRun 'ipfs2fil push --out %s' to re-upload what a copy lost", err, o.out)
	}
	return nil
}
