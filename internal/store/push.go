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

// ArchiveKey is the key Store methods take: the root of the first snapshot.
func ArchiveKey(m *manifest.Manifest) (string, error) {
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
	Skipped bool  // the copy already held this piece
	Err     error // this copy stopped at this piece
}

// Push stores every piece in copies data sets. Pieces a data set already
// holds are skipped, so a re-run after a crash or an edit uploads only what is
// missing. Copies upload in parallel. m.Copies is updated as pieces land, and
// save is called after each one so a killed run loses at most the pieces in
// flight. Each piece carries ipfsRootCID = m.Root.
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
	errs := make([]error, len(sets))
	var wg sync.WaitGroup
	for i, ds := range sets {
		wg.Go(func() {
			for _, p := range pieces {
				ev := PushEvent{Copy: i, Provider: ds.Provider, Piece: p.Index, Total: p.CARBytes}
				skipped, id, err := pushOne(ctx, s, ds, p, meta, func(done int64) {
					ev.Done = done
					progress(ev)
				})
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
			return i
		}
	}
	m.Copies = append(m.Copies, manifest.Copy{Provider: ds.Provider, DataSetID: ds.ID, PieceIDs: map[int]string{}, Service: service})
	return len(m.Copies) - 1
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
