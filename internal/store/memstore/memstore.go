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
	uploads int
	pulls   int
	// PullFails makes every Pull fail, so Push falls back to uploading.
	PullFails bool
}

var (
	_ store.Store  = (*Store)(nil)
	_ store.Puller = (*Store)(nil)
)

func New(providers int) *Store {
	return &Store{Providers: providers, pieces: map[string]map[string]string{},
		data: map[string][]byte{}, meta: map[string]map[string]string{}}
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

func (s *Store) Upload(_ context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (store.PieceRef, error) {
	ref := store.PieceRef{DataSetID: ds.ID, PieceCID: pieceCID}
	s.mu.Lock()
	p, ok := s.pieces[ds.ID]
	if id, held := p[pieceCID]; held {
		s.mu.Unlock()
		ref.PieceID = id
		return ref, nil
	}
	s.mu.Unlock()
	if !ok {
		return ref, fmt.Errorf("no data set %s", ds.ID)
	}
	b, err := io.ReadAll(car)
	if err != nil {
		return ref, err
	}
	if int64(len(b)) != size {
		return ref, fmt.Errorf("piece %s: read %d bytes, expected %d", pieceCID, len(b), size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ref.PieceID = strconv.Itoa(len(p))
	p[pieceCID] = ref.PieceID
	s.data[pieceCID] = b
	s.meta[ds.ID+"/"+pieceCID] = meta
	s.uploads++
	return ref, nil
}

func (s *Store) Pull(_ context.Context, ds, from store.DataSet, pieceCID string, meta map[string]string) (store.PieceRef, error) {
	ref := store.PieceRef{DataSetID: ds.ID, PieceCID: pieceCID}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pieces[ds.ID]
	if !ok {
		return ref, fmt.Errorf("no data set %s", ds.ID)
	}
	if id, held := p[pieceCID]; held {
		ref.PieceID = id
		return ref, nil
	}
	if s.PullFails {
		return ref, fmt.Errorf("provider %s could not fetch piece %s", ds.Provider, pieceCID)
	}
	if _, ok := s.pieces[from.ID][pieceCID]; !ok {
		return ref, fmt.Errorf("data set %s has no piece %s to pull", from.ID, pieceCID)
	}
	ref.PieceID = strconv.Itoa(len(p))
	p[pieceCID] = ref.PieceID
	s.meta[ds.ID+"/"+pieceCID] = meta
	s.pulls++
	return ref, nil
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
