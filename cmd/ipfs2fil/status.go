package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
)

func init() {
	register(&command{
		name:    "status",
		summary: "Show copies, funding and proofs for an archive",
		help: `
Reads the archive's data sets from chain: which providers hold a copy, how
many pieces each has, when each last proved it still holds them, and how many
days the Filecoin Pay deposit lasts at today's rate. It reports only what the
chain says. No wallet key needed: --address (or $IPFS2FIL_ADDRESS) is enough.
With --json, prints one JSON object.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			repoOnlyFlag(fs, o)
			outFlag(fs, o)
			jsonFlag(fs, o)
			addReadStoreFlags(fs)
		},
		run: runStatus,
	})
}

type statusResult struct {
	Archive    string                `json:"archive"`
	Pieces     int                   `json:"pieces"`
	DataSets   []store.DataSetStatus `json:"dataSets"`
	FundedDays int                   `json:"fundedDays"` // -1 if the store cannot say
}

func runStatus(ctx context.Context, o *opts, _ []string) error {
	m, err := manifest.Read(manifestPath(o.out))
	if err != nil {
		_, _, err = loadArchive(o.out) // for its friendlier error
		return err
	}
	key, err := store.ArchiveKey(m)
	if err != nil {
		return err
	}
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()
	st, err := s.Status(ctx, key)
	if err != nil {
		return err
	}
	res := statusResult{Archive: m.Root, Pieces: len(m.Pieces), DataSets: st.DataSets, FundedDays: st.FundedDays}
	if res.DataSets == nil {
		res.DataSets = []store.DataSetStatus{}
	}
	if o.json {
		e := json.NewEncoder(o.stdout)
		e.SetIndent("", "  ")
		return e.Encode(res)
	}
	w := o.stdout
	fmt.Fprintf(w, "archive  %s (%d pieces)\n", res.Archive, res.Pieces)
	if len(st.DataSets) == 0 {
		fmt.Fprintf(w, "copies   none yet; run 'ipfs2fil push --out %s'\n", o.out)
		return nil
	}
	fmt.Fprintf(w, "copies   %d\n", len(st.DataSets))
	for i, d := range st.DataSets {
		note := ""
		if d.Pieces < res.Pieces {
			note = fmt.Sprintf("  (missing %d; run push)", res.Pieces-d.Pieces)
		}
		fmt.Fprintf(w, "  %d. provider %s, data set %s: %d pieces, last proven %s%s\n", i+1, d.Provider, d.ID, d.Pieces, d.LastProven, note)
	}
	switch {
	case st.FundedDays < 0:
	case st.FundedDays < 30:
		fmt.Fprintf(w, "funded   %d days; top up the Filecoin Pay deposit soon or providers stop storing\n", st.FundedDays)
	default:
		fmt.Fprintf(w, "funded   %d days\n", st.FundedDays)
	}
	return nil
}
