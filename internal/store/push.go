package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// ArchiveKey is the key Store methods take: the root of the first snapshot,
// or for an archive in a collection, the collection's key, so every archive
// in it shares one set of data sets.
func ArchiveKey(m *manifest.Manifest) (string, error) {
	if m.Collection != "" {
		return m.Collection, nil
	}
	if len(m.Snapshots) == 0 || m.Snapshots[0].Root == "" {
		return "", errors.New("the manifest has no snapshots; run pack first")
	}
	return m.Snapshots[0].Root, nil
}

// PushEvent reports progress on one piece in one copy. Events for different
// copies arrive from different goroutines.
type PushEvent struct {
	Copy     int    // index into the data sets, 0..copies-1
	Provider string // the copy's provider
	Piece    int    // manifest piece index
	// Done and Total are CAR bytes sent so far and in all. Done reaches
	// Total when the bytes are sent, before the piece is committed.
	Done, Total int64
	// Final marks the last event for this piece in this copy: it is stored
	// (or Skipped), or Err says why not.
	Final   bool
	Skipped bool // the copy already held this piece
	// Pulled is set when the copy's provider fetched the piece from the
	// first copy's provider (From) instead of from this machine.
	Pulled bool
	From   string
	// PullErr says why a provider could not fetch the piece from the first
	// copy, on a non-final event; the piece is then uploaded from here.
	PullErr error
	Err     error // this copy stopped at this piece
}

// Puller is a Store whose providers can fetch a piece from one another. Push
// then sends each piece from this machine once, to the first copy, and the
// other copies' providers fetch it from there.
type Puller interface {
	// Pull has ds's provider fetch the piece from the provider of from,
	// which holds it, and adds it to ds. If ds already holds the piece, Pull
	// returns its ref.
	Pull(ctx context.Context, ds, from DataSet, pieceCID string, meta map[string]string) (PieceRef, error)
}

// Push stores every piece in copies data sets. Pieces a data set already
// holds are skipped, so a re-run after a crash or an edit uploads only what is
// missing. Copies upload in parallel. m.Copies is updated as pieces land, and
// save is called after each one so a killed run loses at most the pieces in
// flight. Each piece carries ipfsRootCID = m.Root.
//
// When s is a Puller, the first copy uploads each piece and the others pull
// it from the first copy's provider once it lands, falling back to an upload
// if the pull fails or the first copy does not get the piece.
//
// A failing copy stops at its first error; the other copies carry on, and
// Push returns all errors joined.
func Push(ctx context.Context, s Store, m *manifest.Manifest, pieces []archive.Piece, copies int,
	save func(*manifest.Manifest) error, progress func(PushEvent)) error {
	key, err := ArchiveKey(m)
	if err != nil {
		return err
	}
	if progress == nil {
		progress = func(PushEvent) {}
	}
	sets, err := s.DataSets(ctx, key, copies)
	if err != nil {
		return err
	}
	service := "unknown"
	if n, ok := s.(interface{ Service() string }); ok {
		service = n.Service()
	}

	var mu sync.Mutex                 // guards m and save
	records := make([]int, len(sets)) // index into m.Copies
	mu.Lock()
	for i, ds := range sets {
		records[i] = copyRecord(m, ds, service)
	}
	err = save(m)
	mu.Unlock()
	if err != nil {
		return fmt.Errorf("saving the manifest: %w", err)
	}

	meta := map[string]string{"ipfsRootCID": m.Root}
	// landed[j] closes when the first copy is done with pieces[j]; held[j]
	// then says whether it holds it, so the other copies can pull it.
	puller, _ := s.(Puller)
	var landed []chan struct{}
	var held []bool
	if puller != nil && len(sets) > 1 {
		landed, held = make([]chan struct{}, len(pieces)), make([]bool, len(pieces))
		for j := range landed {
			landed[j] = make(chan struct{})
		}
	}
	errs := make([]error, len(sets))
	var wg sync.WaitGroup
	for i, ds := range sets {
		wg.Go(func() {
			next := 0
			if landed != nil && i == 0 {
				defer func() {
					for _, c := range landed[next:] {
						close(c)
					}
				}()
			}
			for j, p := range pieces {
				ev := PushEvent{Copy: i, Provider: ds.Provider, Piece: p.Index, Total: p.CARBytes}
				sent := func(done int64) {
					ev.Done = done
					progress(ev)
				}
				var skipped bool
				var id string
				var err error
				if landed != nil && i > 0 {
					skipped, id, err = pullOne(ctx, puller, s, ds, sets[0], p, meta, landed[j], &held[j], &ev, progress, sent)
				} else {
					skipped, id, err = pushOne(ctx, s, ds, p, meta, sent)
				}
				if landed != nil && i == 0 {
					held[j] = err == nil
					close(landed[j])
					next = j + 1
				}
				if err != nil {
					ev.Err, ev.Final = err, true
					progress(ev)
					errs[i] = fmt.Errorf("copy %d on provider %s: %w", i+1, ds.Provider, err)
					return
				}
				mu.Lock()
				ids := m.Copies[records[i]].PieceIDs
				old := ids[p.Index]
				ids[p.Index] = id
				if old != id {
					err = save(m)
				}
				mu.Unlock()
				if err != nil {
					errs[i] = fmt.Errorf("saving the manifest: %w", err)
					return
				}
				ev.Done, ev.Skipped, ev.Final = p.CARBytes, skipped, true
				progress(ev)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// copyRecord finds or adds m's Copy for ds and returns its index.
func copyRecord(m *manifest.Manifest, ds DataSet, service string) int {
	for i := range m.Copies {
		if c := &m.Copies[i]; c.DataSetID == ds.ID && c.Provider == ds.Provider {
			if c.PieceIDs == nil {
				c.PieceIDs = map[int]string{}
			}
			if c.Service == "" {
				c.Service = service
			}
			return i
		}
	}
	m.Copies = append(m.Copies, manifest.Copy{Provider: ds.Provider, DataSetID: ds.ID, PieceIDs: map[int]string{}, Service: service})
	return len(m.Copies) - 1
}

// pullOne stores p in ds by having its provider fetch it from the first copy,
// from, once that holds it; it uploads p instead if the pull fails or the
// first copy did not get it.
func pullOne(ctx context.Context, puller Puller, s Store, ds, from DataSet, p archive.Piece, meta map[string]string,
	landed <-chan struct{}, held *bool, ev *PushEvent, progress func(PushEvent), sent func(int64)) (skipped bool, id string, err error) {
	has, err := s.Has(ctx, ds, p.PieceCID)
	if err != nil {
		return false, "", err
	}
	if has {
		return pushOne(ctx, s, ds, p, meta, sent)
	}
	select {
	case <-landed:
	case <-ctx.Done():
		return false, "", ctx.Err()
	}
	if *held {
		ref, err := puller.Pull(ctx, ds, from, p.PieceCID, meta)
		if err == nil {
			ev.Pulled, ev.From = true, from.Provider
			return false, ref.PieceID, nil
		}
		if ctx.Err() != nil {
			return false, "", err
		}
		progress(PushEvent{Copy: ev.Copy, Provider: ev.Provider, Piece: ev.Piece, Total: ev.Total, From: from.Provider, PullErr: err})
	}
	return pushOne(ctx, s, ds, p, meta, sent)
}

func pushOne(ctx context.Context, s Store, ds DataSet, p archive.Piece, meta map[string]string, sent func(int64)) (skipped bool, id string, err error) {
	has, err := s.Has(ctx, ds, p.PieceCID)
	if err != nil {
		return false, "", err
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return false, "", fmt.Errorf("piece %d: %w", p.Index, err)
	}
	defer f.Close()
	var r io.Reader = f
	if !has {
		r = &counter{r: f, report: sent}
	}
	// Upload returns the existing ref without reading when ds holds the piece.
	ref, err := s.Upload(ctx, ds, p.PieceCID, r, p.CARBytes, meta)
	if err != nil {
		return false, "", err
	}
	return has, ref.PieceID, nil
}

// counter reports bytes read every 256 KiB, and once more at EOF.
type counter struct {
	r      io.Reader
	n, at  int64
	report func(int64)
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if c.n-c.at >= 256<<10 || (err == io.EOF && c.n > c.at) {
		c.at = c.n
		c.report(c.n)
	}
	return n, err
}
