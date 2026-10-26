package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
)

// --all: pack, push, up and status over every repo in the collection.

// listedRepos opens the collection and fails, saying what to run, when this
// machine lists no repos.
func listedRepos(o *opts) (*collection.Collection, error) {
	c, err := openCollection(o)
	if err != nil {
		return nil, err
	}
	if len(c.Repos) == 0 {
		return nil, fmt.Errorf("no repos on this machine in %s yet; add them with 'ipfs2fil repos add <path>'", c.Where())
	}
	return c, nil
}

// publish copies a repo's manifest and block lists to the catalog, if any;
// a failure only warns, since the archive itself is fine.
func publish(o *opts, c *collection.Collection, r collection.Repo) {
	if err := c.Publish(r); err != nil {
		fmt.Fprintf(o.stderr, "%s: could not update the catalog in %s: %v\n", r.Name, c.Catalog, err)
	}
}

// forRepo is o pointed at one repo of the collection.
func forRepo(o *opts, c *collection.Collection, r collection.Repo) *opts {
	ro := *o
	ro.repo, ro.out, ro.collectionKey = r.Path, c.OutDir(r), c.Key
	return &ro
}

// packAll packs every repo in order, each leaving out the blocks earlier
// repos' pieces hold. A repo that fails does not stop the others.
func packAll(ctx context.Context, o *opts) error {
	c, err := listedRepos(o)
	if err != nil {
		return err
	}
	var errs []error
	for i, r := range c.Repos {
		fmt.Fprintf(o.stdout, "== %s (%d of %d)\n", r.Name, i+1, len(c.Repos))
		ro := forRepo(o, c, r)
		if ro.shared, err = c.Shared(r.Name); err == nil {
			if _, err = pack(ctx, ro); err == nil {
				publish(o, c, r)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			fmt.Fprintf(o.stderr, "%s: %v\n", r.Name, err)
			errs = append(errs, fmt.Errorf("%s: %w", r.Name, err))
		}
		fmt.Fprintln(o.stdout)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d repos did not pack:\n%w", len(errs), len(c.Repos), errors.Join(errs...))
	}
	fmt.Fprintln(o.stdout, "next     ipfs2fil push --all")
	return nil
}

type listedArchive struct {
	repo   collection.Repo
	o      *opts
	m      *manifest.Manifest
	pieces []archive.Piece
	key    string
}

// pushAll pushes every packed repo in order, after one confirmation per set
// of data sets (one, unless some archive was pushed before it joined).
func pushAll(ctx context.Context, o *opts) error {
	c, err := listedRepos(o)
	if err != nil {
		return err
	}
	var as []*listedArchive
	for _, r := range c.Repos {
		ro := forRepo(o, c, r)
		m, pieces, err := loadArchive(ro.out)
		if err != nil {
			fmt.Fprintf(o.stderr, "%s: not packed yet, skipping; run 'ipfs2fil pack --all'\n", r.Name)
			continue
		}
		key, err := store.ArchiveKey(m)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Name, err)
		}
		as = append(as, &listedArchive{repo: r, o: ro, m: m, pieces: pieces, key: key})
	}
	if len(as) == 0 {
		return errors.New("no repo is packed yet; run 'ipfs2fil pack --all' or 'ipfs2fil up --all'")
	}
	adoptCopies(as)

	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()

	// What is missing, per data set key, in the order keys first appear.
	var keys []string
	sizes := map[string]map[string]int64{}
	totals := map[string]int64{}
	var todo []*listedArchive
	for _, a := range as {
		n, err := missing(ctx, s, a.m, a.pieces, o.copies)
		if err != nil {
			return fmt.Errorf("%s: %w", a.repo.Name, err)
		}
		if n == 0 {
			fmt.Fprintf(o.stdout, "%s: all %d pieces already stored in %d copies\n", a.repo.Name, len(a.pieces), o.copies)
			publish(o, c, a.repo)
			continue
		}
		todo = append(todo, a)
		if sizes[a.key] == nil {
			keys = append(keys, a.key)
			sizes[a.key] = map[string]int64{}
		}
		for _, p := range a.pieces {
			if _, dup := sizes[a.key][p.PieceCID]; !dup {
				sizes[a.key][p.PieceCID] = p.CARBytes
				totals[a.key] += p.CARBytes
			}
		}
	}
	if len(todo) == 0 {
		fmt.Fprintf(o.stdout, "all %d packed repos are stored; nothing to upload\n", len(as))
		return nil
	}
	for _, k := range keys {
		if ok, err := approveCost(ctx, o, s, k, sizes[k], totals[k]); err != nil || !ok {
			if err == nil {
				fmt.Fprintln(o.stderr, "nothing uploaded")
			}
			return err
		}
	}

	var errs []error
	for i, a := range todo {
		fmt.Fprintf(o.stdout, "\n== %s (%d of %d)\n", a.repo.Name, i+1, len(todo))
		var total int64
		for _, p := range a.pieces {
			total += p.CARBytes
		}
		err := upload(ctx, a.o, s, a.m, a.pieces, total)
		publish(o, c, a.repo)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			fmt.Fprintf(o.stderr, "%s: %v\n", a.repo.Name, err)
			errs = append(errs, fmt.Errorf("%s: %w", a.repo.Name, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d repos did not finish; run 'ipfs2fil push --all' again to retry:\n%w", len(errs), len(todo), errors.Join(errs...))
	}
	fmt.Fprintln(o.stdout, "\nnext     ipfs2fil status --all   (proofs start within a day)")
	return nil
}

// adoptCopies gives an archive that has no copies yet the data sets another
// archive with the same key already uses, so push checks them for pieces it
// has and says no new data sets are needed. Piece ids fill in as it pushes.
func adoptCopies(as []*listedArchive) {
	for _, a := range as {
		if len(a.m.Copies) > 0 {
			continue
		}
		for _, b := range as {
			if b.key == a.key && len(b.m.Copies) > 0 {
				for _, cp := range b.m.Copies {
					a.m.Copies = append(a.m.Copies, manifest.Copy{Provider: cp.Provider, DataSetID: cp.DataSetID,
						Service: cp.Service, PieceIDs: map[int]string{}})
				}
				break
			}
		}
	}
}

func upAll(ctx context.Context, o *opts) error {
	if err := packAll(ctx, o); err != nil {
		return err
	}
	fmt.Fprintln(o.stdout)
	return pushAll(ctx, o)
}

type statusAllResult struct {
	Home  string          `json:"home"`
	Repos []repoStatusRow `json:"repos"`
	statusResult
}

type repoStatusRow struct {
	repoRow
	Stored int `json:"stored"` // pieces every copy holds
}

// statusAll shows every repo, on every machine in the catalog, how much of
// it each copy holds, and the shared data sets' proofs and funding.
func statusAll(ctx context.Context, o *opts) error {
	c, err := openCollection(o)
	if err != nil {
		return err
	}
	es, err := c.Entries()
	if err != nil {
		return err
	}
	if len(es) == 0 {
		return fmt.Errorf("no repos in %s yet; add them with 'ipfs2fil repos add <path>'", c.Where())
	}
	rows, err := repoRows(c, es)
	if err != nil {
		return err
	}
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()
	statuses := map[string]store.Status{}
	statusOf := func(key string) (store.Status, error) {
		if st, ok := statuses[key]; ok {
			return st, nil
		}
		st, err := s.Status(ctx, key)
		statuses[key] = st
		return st, err
	}
	main, err := statusOf(c.Key)
	if err != nil {
		return err
	}
	res := statusAllResult{Home: c.Dir, Repos: []repoStatusRow{}}
	res.Archive, res.DataSets, res.FundedDays = c.Key, []dataSetResult{}, main.FundedDays
	res.Reserve, res.ReserveDataSets = main.Reserve, main.ReserveDataSets
	distinct := map[string]bool{}
	for i, e := range es {
		row := repoStatusRow{repoRow: rows[i]}
		m, err := e.Manifest()
		if err != nil {
			return err
		}
		if m != nil {
			key, err := store.ArchiveKey(m)
			if err != nil {
				return err
			}
			st, err := statusOf(key)
			if err != nil {
				return err
			}
			for _, p := range m.Pieces {
				if key == c.Key {
					distinct[p.PieceCID] = true
				}
				all := len(st.DataSets) > 0
				for _, d := range st.DataSets {
					ok, err := s.Has(ctx, d.DataSet, p.PieceCID)
					if err != nil {
						return err
					}
					all = all && ok
				}
				if all {
					row.Stored++
				}
			}
		}
		res.Repos = append(res.Repos, row)
	}
	res.Pieces = len(distinct)
	for _, d := range main.DataSets {
		res.DataSets = append(res.DataSets, dataSetResult{Provider: d.Provider, ID: d.ID, URL: d.URL,
			Pieces: d.Pieces, LastProven: d.LastProven, Metadata: d.Metadata})
	}
	if o.json {
		e := json.NewEncoder(o.stdout)
		e.SetIndent("", "  ")
		return e.Encode(res)
	}

	w := o.stdout
	fmt.Fprintf(w, "%d repos (%d on this machine)\n\n", len(res.Repos), len(c.Repos))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTORED\tSIZE\tSHARED")
	var bytes, saved int64
	for _, r := range res.Repos {
		stored, size, shared := "not packed", "", ""
		if r.Archive != "" {
			stored, size = fmt.Sprintf("%d of %d pieces", r.Stored, r.Pieces), humanBytes(r.Bytes)
			if r.OwnDataSets {
				stored += " (own data sets)"
			}
			bytes += r.Bytes
		}
		if r.SharedBytes > 0 {
			shared = humanBytes(r.SharedBytes)
			saved += r.SharedBytes
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, stored, size, shared)
	}
	tw.Flush()
	fmt.Fprintf(w, "\ntotal    %s in pieces", humanBytes(bytes))
	if saved > 0 {
		fmt.Fprintf(w, "; %s shared between repos is stored once", humanBytes(saved))
	}
	fmt.Fprintln(w)
	if len(main.DataSets) == 0 {
		fmt.Fprintln(w, "copies   none yet; run 'ipfs2fil up --all'")
		return nil
	}
	fmt.Fprintf(w, "copies   %d data sets shared by every repo\n", len(main.DataSets))
	for i, d := range main.DataSets {
		note := ""
		if d.Pieces < res.Pieces {
			note = fmt.Sprintf("  (missing %d; run push --all)", res.Pieces-d.Pieces)
		}
		fmt.Fprintf(w, "  %d. provider %s, data set %s: %d pieces, last proven %s%s\n", i+1, d.Provider, d.ID, d.Pieces, d.LastProven, note)
	}
	printFunding(w, main)
	return nil
}
