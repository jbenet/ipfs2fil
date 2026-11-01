package main

import (
	"context"
	"fmt"
	"slices"

	"github.com/jbenet/ipfs2fil/internal/car"
	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
)

// Removing pieces earlier snapshots used.
//
// A snapshot keeps the stored pieces it still uses (see archive.KeepLive)
// and drops the rest: the last snapshot's head, the index and explorer
// parts that changed, and data pieces too little of which the repo still
// has, whose live blocks went into new pieces. Filecoin Onchain Cloud keeps
// a piece, and charges for it, until it is removed, so push removes the
// pieces this machine's archives dropped once no repo on any machine uses
// them and they add up to enough to pay for the step. Only pieces this
// machine's archives used are removed: another machine's pieces may belong
// to a push still under way.

// removeMin is how many bytes of dropped pieces make removing them worth an
// on-chain step: 512 MiB in 3 copies costs about 0.011 USDFC a month, and a
// removal step about 0.007 per copy, so it pays for itself within two months.
const removeMin = 512 << 20

// droppedArchive is one of this machine's archives, with the pieces it no
// longer uses.
type droppedArchive struct {
	name string
	out  string
	m    *manifest.Manifest
}

// removal is what a push can remove from a set of data sets.
type removal struct {
	sets  []store.DataSet
	ids   [][]string // per data set
	cids  []string
	bytes int64
}

// planRemoval finds the pieces in key's data sets that the archives dropped
// and no archive in use lists, as its own or as one it shares blocks from.
func planRemoval(ctx context.Context, s store.Store, key string, copies int, as []droppedArchive, use []*manifest.Manifest) (*removal, error) {
	l, ok := s.(store.Lister)
	if !ok {
		return nil, nil
	}
	if _, ok := s.(store.Remover); !ok {
		return nil, nil
	}
	used := map[string]bool{}
	for _, m := range use {
		for _, p := range m.Pieces {
			used[p.PieceCID] = true
		}
		if m.Shared != nil {
			for _, p := range m.Shared.Pieces {
				used[p.PieceCID] = true
			}
		}
	}
	want := map[string]bool{}
	for _, a := range as {
		for _, c := range a.m.Dropped {
			if !used[c] {
				want[c] = true
			}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	sets, held, err := l.Pieces(ctx, key, copies)
	if err != nil {
		return nil, err
	}
	r := &removal{sets: sets, ids: make([][]string, len(sets))}
	counted := map[string]bool{}
	for i, h := range held {
		for c, id := range h {
			if !want[c] {
				continue
			}
			r.ids[i] = append(r.ids[i], id)
			if !counted[c] {
				counted[c] = true
				r.cids = append(r.cids, c)
				r.bytes += car.PayloadSize(c)
			}
		}
		slices.SortFunc(r.ids[i], cmpID)
	}
	slices.Sort(r.cids)
	return r, nil
}

// cmpID orders decimal piece ids numerically.
func cmpID(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// removeDropped removes what planRemoval finds, after asking, when it is
// worth a step or --remove-earlier says to; then it forgets, in each
// archive's manifest, the dropped pieces no data set holds any more.
func removeDropped(ctx context.Context, o *opts, s store.Store, key string, as []droppedArchive, use []*manifest.Manifest) error {
	if o.keepEarlier {
		return nil
	}
	r, err := planRemoval(ctx, s, key, o.copies, as, use)
	if err != nil || r == nil {
		return err
	}
	w := o.stdout
	if len(r.cids) > 0 {
		if r.bytes < removeMin && !o.removeEarlier {
			fmt.Fprintf(w, "earlier  %d %s (%s) earlier snapshots used stay stored until they pass %s; --remove-earlier removes them now\n",
				len(r.cids), plural(len(r.cids), "piece", "pieces"), humanBytes(r.bytes), humanBytes(removeMin))
			return nil
		}
		monthly := store.Estimate(r.bytes, 0, len(r.sets)).Monthly
		fmt.Fprintf(o.stderr, "removing %d %s (%s) that earlier snapshots used and no repo uses now saves about %s USDFC/month, for about %s USDFC once; those snapshots can then no longer be read back in full\n",
			len(r.cids), plural(len(r.cids), "piece", "pieces"), humanBytes(r.bytes), usdfc(monthly), usdfc(0.007*float64(len(r.sets))))
		ok, err := confirm(o, stdin, "Remove them?")
		if err != nil || !ok {
			return err
		}
		rm := s.(store.Remover)
		for i, ds := range r.sets {
			if len(r.ids[i]) == 0 {
				continue
			}
			if err := rm.Remove(ctx, ds, r.ids[i]); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "removed  %d %s (%s) of earlier snapshots from %d %s\n", len(r.cids), plural(len(r.cids), "piece", "pieces"),
			humanBytes(r.bytes), len(r.sets), plural(len(r.sets), "copy", "copies"))
	}
	// What no data set holds now, nothing needs to remove.
	l := s.(store.Lister)
	_, held, err := l.Pieces(ctx, key, o.copies)
	if err != nil {
		return err
	}
	for _, a := range as {
		var keep []string
		for _, c := range a.m.Dropped {
			for _, h := range held {
				if _, ok := h[c]; ok {
					keep = append(keep, c)
					break
				}
			}
		}
		if len(keep) == len(a.m.Dropped) {
			continue
		}
		m, err := manifest.Read(manifestPath(a.out))
		if err != nil {
			return err
		}
		m.Dropped = keep
		if err := m.Write(manifestPath(a.out)); err != nil {
			return err
		}
	}
	return nil
}

// collectionUse lists the current manifests of every repo the catalog
// knows, on every machine. ok is false if one cannot be read: then nothing
// can be known to be unused.
func collectionUse(c *collection.Collection) ([]*manifest.Manifest, bool) {
	es, err := c.Entries()
	if err != nil {
		return nil, false
	}
	var out []*manifest.Manifest
	for _, e := range es {
		m, err := e.Manifest()
		if err != nil {
			return nil, false
		}
		if m != nil {
			out = append(out, m)
		}
	}
	return out, true
}
