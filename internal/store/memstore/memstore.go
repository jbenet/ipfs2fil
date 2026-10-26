// Package memstore is an in-memory store.Store for tests. It behaves like the
// FOC store where tests can see it: one data set per copy on distinct
// providers, keyed by archive, pieces skipped when already held.
package memstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"strconv"
	"sync"

	"github.com/jbenet/ipfs2fil/internal/store"
)

type Store struct {
	// Providers is how many providers exist; DataSets fails past it.
	Providers int

	mu      sync.Mutex
	sets    []store.DataSet
	pieces  map[string]map[string]string // data set id -> PieceCID -> piece id
	data    map[string][]byte            // PieceCID -> bytes
	meta    map[string]map[string]string // data set id/PieceCID -> metadata
	staged  map[string]map[string]bool   // provider -> PieceCID staged, not yet added
	uploads int
	pulls   int
	commits int
	// PullFails makes every Pull fail, so Push falls back to uploading.
	PullFails bool
	// BeforeCommit, if set, runs before each Commit that adds pieces, as a
	// wait for the base fee; an error fails the Commit.
	BeforeCommit func(ctx context.Context, ds store.DataSet, pieceCIDs []string) error
}

var _ store.Store = (*Store)(nil)

func New(providers int) *Store {
	return &Store{Providers: providers, pieces: map[string]map[string]string{},
		data: map[string][]byte{}, meta: map[string]map[string]string{}, staged: map[string]map[string]bool{}}
}

// Commits counts Commit calls that added pieces.
func (s *Store) Commits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits
}

// Drop makes provider forget a staged piece, as FOC providers do with pieces
// not added within a couple of hours.
func (s *Store) Drop(provider, pieceCID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.staged[provider], pieceCID)
}

// Staged reports whether provider holds the piece staged, not yet added.
func (s *Store) Staged(provider, pieceCID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged[provider][pieceCID]
}

func (s *Store) Service() string { return "mem" }

// Uploads counts pieces actually stored, by upload or pull; skips excluded.
func (s *Store) Uploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploads + s.pulls
}

// Pulls counts pieces a provider fetched from another provider.
func (s *Store) Pulls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pulls
}

// Meta returns the metadata a piece was uploaded with.
func (s *Store) Meta(dataSetID, pieceCID string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta[dataSetID+"/"+pieceCID]
}

// Corrupt flips a byte of a stored piece, so Fetch returns bad data.
func (s *Store) Corrupt(pieceCID string, at int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := bytes.Clone(s.data[pieceCID])
	b[at] ^= 0xff
	s.data[pieceCID] = b
}

func (s *Store) DataSets(_ context.Context, archive string, copies int) ([]store.DataSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.DataSet
	used := map[string]bool{}
	for _, d := range s.sets {
		if d.Metadata["ipfs2fil"] == archive && len(out) < copies {
			out = append(out, d)
			used[d.Provider] = true
		}
	}
	for p := 1; len(out) < copies && p <= s.Providers; p++ {
		if used[strconv.Itoa(p)] {
			continue
		}
		d := store.DataSet{Provider: strconv.Itoa(p), ID: strconv.Itoa(100 + len(s.sets)),
			Metadata: map[string]string{"ipfs2fil": archive, "withIPFSIndexing": ""}}
		s.sets = append(s.sets, d)
		s.pieces[d.ID] = map[string]string{}
		out = append(out, d)
	}
	if len(out) < copies {
		return nil, fmt.Errorf("asked for %d copies but only %d providers are available", copies, len(out))
	}
	return out, nil
}

var _ store.Lister = (*Store)(nil)

func (s *Store) Pieces(_ context.Context, archive string, copies int) ([]store.DataSet, []map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sets []store.DataSet
	var pieces []map[string]string
	for _, d := range s.sets {
		if d.Metadata["ipfs2fil"] == archive && len(sets) < copies {
			sets = append(sets, d)
			pieces = append(pieces, maps.Clone(s.pieces[d.ID]))
		}
	}
	return sets, pieces, nil
}

func (s *Store) Has(_ context.Context, ds store.DataSet, pieceCID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pieces[ds.ID][pieceCID]
	return ok, nil
}

func (s *Store) Upload(ctx context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (store.PieceRef, error) {
	ref := store.PieceRef{DataSetID: ds.ID, PieceCID: pieceCID}
	if err := s.Stage(ctx, ds, pieceCID, car, size); err != nil {
		return ref, err
	}
	ids, err := s.Commit(ctx, ds, []string{pieceCID}, meta)
	if err != nil {
		return ref, err
	}
	ref.PieceID = ids[0]
	return ref, nil
}

func (s *Store) Stage(_ context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64) error {
	s.mu.Lock()
	p, ok := s.pieces[ds.ID]
	_, held := p[pieceCID]
	staged := s.staged[ds.Provider][pieceCID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no data set %s", ds.ID)
	}
	if held || staged {
		return nil
	}
	b, err := io.ReadAll(car)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("piece %s: read %d bytes, expected %d", pieceCID, len(b), size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[pieceCID] = b
	s.stage(ds.Provider, pieceCID)
	s.uploads++
	return nil
}

// stage marks the piece staged with provider; the caller holds s.mu.
func (s *Store) stage(provider, pieceCID string) {
	if s.staged[provider] == nil {
		s.staged[provider] = map[string]bool{}
	}
	s.staged[provider][pieceCID] = true
}

func (s *Store) Pull(_ context.Context, ds, from store.DataSet, pieceCID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pieces[ds.ID]
	if !ok {
		return fmt.Errorf("no data set %s", ds.ID)
	}
	if _, held := p[pieceCID]; held || s.staged[ds.Provider][pieceCID] {
		return nil
	}
	if s.PullFails {
		return fmt.Errorf("provider %s could not fetch piece %s", ds.Provider, pieceCID)
	}
	if _, ok := s.pieces[from.ID][pieceCID]; !ok && !s.staged[from.Provider][pieceCID] {
		return fmt.Errorf("provider %s has no piece %s to pull", from.Provider, pieceCID)
	}
	s.stage(ds.Provider, pieceCID)
	s.pulls++
	return nil
}

func (s *Store) Commit(ctx context.Context, ds store.DataSet, pieceCIDs []string, meta map[string]string) ([]string, error) {
	s.mu.Lock()
	p, ok := s.pieces[ds.ID]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("no data set %s", ds.ID)
	}
	adds := false
	for _, c := range pieceCIDs {
		if _, held := p[c]; !held {
			adds = true
		}
	}
	hook := s.BeforeCommit
	s.mu.Unlock()
	if adds && hook != nil {
		if err := hook(ctx, ds, pieceCIDs); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var gone []string
	for _, c := range pieceCIDs {
		if _, held := p[c]; !held && !s.staged[ds.Provider][c] {
			gone = append(gone, c)
		}
	}
	if gone != nil {
		return nil, &store.GoneError{Provider: ds.Provider, PieceCIDs: gone}
	}
	ids := make([]string, len(pieceCIDs))
	for i, c := range pieceCIDs {
		if id, held := p[c]; held {
			ids[i] = id
			continue
		}
		ids[i] = strconv.Itoa(len(p))
		p[c] = ids[i]
		s.meta[ds.ID+"/"+c] = meta
		delete(s.staged[ds.Provider], c)
	}
	if adds {
		s.commits++
	}
	return ids, nil
}

func (s *Store) Fetch(_ context.Context, ds store.DataSet, pieceCID string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pieces[ds.ID][pieceCID]; !ok {
		return nil, fmt.Errorf("data set %s has no piece %s", ds.ID, pieceCID)
	}
	return io.NopCloser(bytes.NewReader(s.data[pieceCID])), nil
}

func (s *Store) Status(_ context.Context, archive string) (store.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st store.Status
	for _, d := range s.sets {
		if d.Metadata["ipfs2fil"] == archive {
			st.DataSets = append(st.DataSets, store.DataSetStatus{DataSet: d, Pieces: len(s.pieces[d.ID]), LastProven: "not yet"})
		}
	}
	st.FundedDays = -1
	return st, nil
}
