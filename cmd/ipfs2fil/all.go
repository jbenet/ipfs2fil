package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/car"
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

// copiesOf is o.copies, or the default for commands without --copies.
func copiesOf(o *opts) int {
	if o.copies > 0 {
		return o.copies
	}
	return 3
}

// readCatalog refreshes the catalog read from chain. Problems only warn:
// what was read before stays usable, and at worst a block another machine
// stores is stored again.
func readCatalog(ctx context.Context, o *opts, c *collection.Collection, s store.Store) {
	if !c.OnChain() {
		return
	}
	secret, err := walletSecret()
	if err != nil {
		fmt.Fprintf(o.stderr, "warning: other machines' repos are read from chain with the wallet key: %v\n", err)
		return
	}
	if err := c.SyncChain(ctx, s, secret, copiesOf(o)); err != nil {
		fmt.Fprintf(o.stderr, "warning: reading the catalog from chain: %v\n(using what was read before)\n", err)
	}
}

// packAll packs every repo in order, each leaving out the blocks earlier
// repos' pieces hold. A repo that fails does not stop the others.
func packAll(ctx context.Context, o *opts) error {
	failed, err := packEach(ctx, o)
	if err != nil {
		return err
	}
	if len(failed.errs) > 0 {
		return failed
	}
	fmt.Fprintln(o.stdout, "next     ipfs2fil push --all")
	return nil
}

// packFailures are the repos packEach could not pack, in order.
type packFailures struct {
	of    int // repos packed
	names map[string]bool
	errs  []error
}

func (f *packFailures) failed(name string) bool { return f != nil && f.names[name] }

func (f *packFailures) Error() string {
	return fmt.Sprintf("%d of %d repos did not pack:\n%v", len(f.errs), f.of, errors.Join(f.errs...))
}

// packEach packs every repo and returns the ones that failed; err is set
// only when nothing more can be done.
func packEach(ctx context.Context, o *opts) (*packFailures, error) {
	c, err := listedRepos(o)
	if err != nil {
		return nil, err
	}
	var secret []byte
	if c.OnChain() {
		// Each archive carries a record naming it, sealed with the wallet
		// key, so other machines find it on chain.
		if secret, err = walletSecret(); err != nil {
			return nil, fmt.Errorf("pack --all names each archive for your other machines under the wallet key: %w", err)
		}
		if s, closer, err := openStore(ctx, o); err != nil {
			fmt.Fprintf(o.stderr, "warning: not reading other machines' repos from chain: %v\n", err)
		} else {
			readCatalog(ctx, o, c, s)
			closer.Close()
		}
	}
	failed := &packFailures{of: len(c.Repos), names: map[string]bool{}}
	// One bundle for all: what changed in every repo goes in as few pieces
	// as fit, written once the last repo is packed.
	bundle := archive.NewBundle(archive.DefaultOptions().MaxPiece)
	defer bundle.Discard()
	var done []collection.Repo
	for i, r := range c.Repos {
		fmt.Fprintf(o.stdout, "== %s (%d of %d)\n", r.Name, i+1, len(c.Repos))
		ro := forRepo(o, c, r)
		if secret != nil {
			if ro.record, err = collection.SealRecord(secret, collection.Record{Host: c.Host, Name: r.Name, Added: r.Added}); err != nil {
				return nil, err
			}
		}
		ro.bundle = bundle
		if ro.shared, err = c.Shared(r.Name); err == nil {
			if _, err = pack(ctx, ro); err == nil {
				done = append(done, r)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			fmt.Fprintf(o.stderr, "%s: %v\n", r.Name, err)
			failed.names[r.Name] = true
			failed.errs = append(failed.errs, fmt.Errorf("%s: %w", r.Name, err))
		}
		fmt.Fprintln(o.stdout)
	}
	pieces, err := bundle.Finish()
	if err != nil {
		return nil, fmt.Errorf("writing the bundle piece of the repos' changes: %w (run it again)", err)
	}
	if len(pieces) > 0 {
		var bytes int64
		for _, p := range pieces {
			bytes += p.CARBytes
		}
		fmt.Fprintf(o.stdout, "bundle   the changes of %d repos in %d piece%s, %s\n\n", bundle.Archives, len(pieces), map[bool]string{true: "s"}[len(pieces) > 1], humanBytes(bytes))
	}
	for _, r := range done {
		publish(o, c, r)
	}
	commitCatalog(o, c, "pack")
	return failed, nil
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
	return pushListed(ctx, o, nil)
}

// pushListed is pushAll, leaving out the repos in skip.
func pushListed(ctx context.Context, o *opts, skip *packFailures) error {
	c, err := listedRepos(o)
	if err != nil {
		return err
	}
	var as []*listedArchive
	for _, r := range c.Repos {
		if skip.failed(r.Name) {
			fmt.Fprintf(o.stderr, "%s: did not pack, so not pushing it\n", r.Name)
			continue
		}
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
	adoptCopies(as, otherCopies(c))

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
			// the copies may be adopted, or a failed run may not have saved
			// them: write them down so status and the explorer see them
			if err := store.Record(ctx, s, a.m, a.pieces, o.copies, saveManifest(a.o.out)); err != nil {
				return fmt.Errorf("%s: %w", a.repo.Name, err)
			}
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
		commitCatalog(o, c, "push")
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
		// an earlier push in this run made the data sets the later ones share
		adoptCopies(todo[:i+1], nil)
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
	commitCatalog(o, c, "push")
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d repos did not finish; run 'ipfs2fil push --all' again to retry:\n%w", len(errs), len(todo), errors.Join(errs...))
	}
	fmt.Fprintln(o.stdout, "\nnext     ipfs2fil status --all   (proofs start within a day)")
	return nil
}

// otherCopies lists the data sets other machines' archives use, by key, as
// the catalog last read them.
func otherCopies(c *collection.Collection) map[string][]manifest.Copy {
	out := map[string][]manifest.Copy{}
	es, err := c.Entries()
	if err != nil {
		return out
	}
	for _, e := range es {
		if e.Local {
			continue
		}
		if m, err := e.Manifest(); err == nil && m != nil && m.Collection != "" && len(m.Copies) > 0 && out[m.Collection] == nil {
			out[m.Collection] = m.Copies
		}
	}
	return out
}

// adoptCopies gives an archive that has no copies yet the data sets another
// archive with the same key already uses, on this machine or (others) on
// another, so push checks them for pieces it has and says no new data sets
// are needed. Piece ids fill in as it pushes.
func adoptCopies(as []*listedArchive, others map[string][]manifest.Copy) {
	for _, a := range as {
		if len(a.m.Copies) > 0 {
			continue
		}
		from := others[a.key]
		for _, b := range as {
			if b.key == a.key && len(b.m.Copies) > 0 {
				from = b.m.Copies
				break
			}
		}
		for _, cp := range from {
			service := cp.Service
			if service == "chain" {
				service = "" // filled in by the store on push
			}
			a.m.Copies = append(a.m.Copies, manifest.Copy{Provider: cp.Provider, DataSetID: cp.DataSetID,
				Service: service, PieceIDs: map[int]string{}})
		}
	}
}

// upAll packs and pushes every repo. A repo that fails to pack does not hold
// back the others: they are pushed, and the failures reported after.
func upAll(ctx context.Context, o *opts) error {
	failed, err := packEach(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintln(o.stdout)
	err = pushListed(ctx, o, failed)
	if len(failed.errs) > 0 {
		return errors.Join(err, fmt.Errorf("%w\nrun 'ipfs2fil up --all' again once fixed; what is done is skipped", failed))
	}
	return err
}

type statusAllResult struct {
	Home  string          `json:"home"`
	Repos []repoStatusRow `json:"repos"`
	// Earlier counts the pieces the data sets hold that no repo's current
	// archive uses: pieces of earlier snapshots. They stay stored and paid for.
	Earlier      int   `json:"earlierPieces"`
	EarlierBytes int64 `json:"earlierBytes"`
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
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()
	readCatalog(ctx, o, c, s)
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
	if l, ok := s.(store.Lister); ok && len(main.DataSets) > 0 {
		_, held, err := l.Pieces(ctx, c.Key, len(main.DataSets))
		if err != nil {
			return err
		}
		earlier := map[string]bool{}
		for _, h := range held {
			for p := range h {
				if !distinct[p] && !earlier[p] {
					earlier[p] = true
					res.EarlierBytes += car.PayloadSize(p)
				}
			}
		}
		res.Earlier = len(earlier)
	}
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
	if res.Earlier > 0 {
		fmt.Fprintf(w, "earlier  %d pieces (%s) in the data sets belong to earlier snapshots, not to any repo's current archive;\n"+
			"         they stay stored and paid for, so those snapshots can still be read\n", res.Earlier, humanBytes(res.EarlierBytes))
	}
	if len(main.Unused) > 0 {
		var ids []string
		for _, d := range main.Unused {
			ids = append(ids, d.ID)
		}
		fmt.Fprintf(w, "unused   data sets %s were made at the same time as the ones above (two machines pushing first at once);\n"+
			"         they hold nothing, and each keeps its share of the reserve until ended\n", strings.Join(ids, ", "))
	}
	printFunding(w, main)
	return nil
}
