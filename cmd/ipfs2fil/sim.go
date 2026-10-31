//go:build sim

// A simulated Filecoin store, for recording the website's terminal demos
// without moving real data or money: go build -tags sim ./cmd/ipfs2fil.
// It keeps its state in $IPFS2FIL_SIM (a JSON file) so that later commands
// see what earlier ones stored, reads every piece at a mainnet-like pace,
// takes on-chain steps at mainnet-like speed, and keeps no piece bytes:
// verify reads pieces back from the archives on this machine. Release
// builds never include it.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jbenet/ipfs2fil/internal/collection"
	"github.com/jbenet/ipfs2fil/internal/ipni"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/foc"
)

// Paces of the simulated network.
const (
	simUpload   = 48 << 20  // bytes per second from this machine to a provider
	simFetch    = 160 << 20 // from a provider to this machine
	simChain    = 30 * time.Second
	simSetup    = 60 * time.Second
	simProvider = 3
)

func init() {
	path := os.Getenv("IPFS2FIL_SIM")
	if path == "" {
		return
	}
	checkKey = func() error { return nil }
	// simulated providers have announced everything
	checkIPNI = func(_ context.Context, _ *http.Client, endpoint string, cids []string, _ int) ipni.Result {
		time.Sleep(2 * time.Second)
		return ipni.Result{Endpoint: strings.TrimRight(endpoint, "/"), Checked: len(cids), Announced: len(cids)}
	}
	walletSecret = func() ([]byte, error) { return []byte("simulated wallet secret, 32 byte"), nil }
	openStore = func(context.Context, *opts) (store.Store, io.Closer, error) {
		s := &simStore{path: path}
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, &s.st)
		} else if os.IsNotExist(err) {
			err = nil
		}
		if s.st.Pieces == nil {
			s.st.Pieces = map[string]map[string]string{}
			s.st.Staged = map[string]map[string]bool{}
		}
		return s, io.NopCloser(nil), err
	}
}

type simState struct {
	Sets      []store.DataSet
	Pieces    map[string]map[string]string // data set id -> PieceCID -> piece id
	Staged    map[string]map[string]bool   // provider -> PieceCID
	Deposited bool
}

type simStore struct {
	path string
	mu   sync.Mutex
	st   simState
}

// save writes the state; the caller holds s.mu.
func (s *simStore) save() {
	b, _ := json.Marshal(&s.st)
	os.WriteFile(s.path, b, 0o644)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func usdfcUnits(x float64) *big.Int {
	f := new(big.Float).Mul(big.NewFloat(x), big.NewFloat(1e18))
	i, _ := f.Int(nil)
	return i
}

func (s *simStore) Plan(_ context.Context, _ string, copies int, pieces map[string]int64) (*foc.Funding, error) {
	var total int64
	for _, n := range pieces {
		total += n
	}
	c := store.Estimate(total, len(pieces), copies)
	s.mu.Lock()
	deposited := s.st.Deposited
	s.mu.Unlock()
	if deposited {
		return foc.SimFunding(nil, usdfcUnits(c.Monthly), usdfcUnits(c.OneTime), usdfcUnits(25), nil), nil
	}
	deposit := usdfcUnits(c.Monthly + c.OneTime + 0.5)
	return foc.SimFunding(deposit, usdfcUnits(c.Monthly), usdfcUnits(c.OneTime), usdfcUnits(25), func(ctx context.Context) error {
		if err := sleep(ctx, simChain); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.st.Deposited = true
		s.save()
		return nil
	}), nil
}

func (s *simStore) Balances(context.Context) (foc.Balances, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := foc.Balances{FIL: usdfcUnits(4.8213), USDFC: usdfcUnits(25), Deposit: big.NewInt(0), Available: big.NewInt(0)}
	if s.st.Deposited {
		b.USDFC, b.Deposit, b.Available = usdfcUnits(23.4), usdfcUnits(1.6), usdfcUnits(0.42)
	}
	return b, nil
}

func (s *simStore) DataSets(ctx context.Context, archive string, copies int) ([]store.DataSet, error) {
	s.mu.Lock()
	var out []store.DataSet
	used := map[string]bool{}
	for _, d := range s.st.Sets {
		if d.Metadata["ipfs2fil"] == archive && len(out) < copies {
			out = append(out, d)
			used[d.Provider] = true
		}
	}
	s.mu.Unlock()
	if len(out) >= copies {
		return out, nil
	}
	if err := sleep(ctx, simSetup); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := 0; len(out) < copies && p < simProvider; p++ {
		id := []string{"2", "9", "16"}[p]
		if used[id] {
			continue
		}
		d := store.DataSet{Provider: id, ID: strconv.Itoa(5310 + len(s.st.Sets)),
			Metadata: map[string]string{"ipfs2fil": archive, "withIPFSIndexing": ""}}
		s.st.Sets = append(s.st.Sets, d)
		s.st.Pieces[d.ID] = map[string]string{}
		out = append(out, d)
	}
	s.save()
	return out, nil
}

func (s *simStore) Pieces(_ context.Context, archive string, copies int) ([]store.DataSet, []map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var sets []store.DataSet
	var pieces []map[string]string
	for _, d := range s.st.Sets {
		if d.Metadata["ipfs2fil"] == archive && len(sets) < copies {
			p := map[string]string{}
			for k, v := range s.st.Pieces[d.ID] {
				p[k] = v
			}
			sets, pieces = append(sets, d), append(pieces, p)
		}
	}
	return sets, pieces, nil
}

func (s *simStore) Has(_ context.Context, ds store.DataSet, pieceCID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.st.Pieces[ds.ID][pieceCID]
	return ok, nil
}

func (s *simStore) Upload(ctx context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (store.PieceRef, error) {
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

// paced reads r no faster than rate bytes per second.
type paced struct {
	ctx   context.Context
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (p *paced) Read(b []byte) (int, error) {
	if len(b) > 1<<20 {
		b = b[:1<<20]
	}
	n, err := p.r.Read(b)
	p.n += int64(n)
	if due := time.Duration(float64(p.n) / float64(p.rate) * float64(time.Second)); due > time.Since(p.start) {
		if e := sleep(p.ctx, due-time.Since(p.start)); e != nil {
			return n, e
		}
	}
	return n, err
}

func (s *simStore) staged(provider, pieceCID string) bool { return s.st.Staged[provider][pieceCID] }

func (s *simStore) stage(provider, pieceCID string) {
	if s.st.Staged[provider] == nil {
		s.st.Staged[provider] = map[string]bool{}
	}
	s.st.Staged[provider][pieceCID] = true
	s.save()
}

func (s *simStore) Stage(ctx context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64) error {
	s.mu.Lock()
	_, held := s.st.Pieces[ds.ID][pieceCID]
	staged := s.staged(ds.Provider, pieceCID)
	s.mu.Unlock()
	if held || staged {
		return nil
	}
	n, err := io.Copy(io.Discard, &paced{ctx: ctx, r: car, rate: simUpload, start: time.Now()})
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("piece %s: read %d bytes, expected %d", pieceCID, n, size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage(ds.Provider, pieceCID)
	return nil
}

func (s *simStore) Pull(ctx context.Context, ds, from store.DataSet, pieceCID string) error {
	s.mu.Lock()
	_, held := s.st.Pieces[ds.ID][pieceCID]
	staged := s.staged(ds.Provider, pieceCID)
	_, there := s.st.Pieces[from.ID][pieceCID]
	there = there || s.staged(from.Provider, pieceCID)
	s.mu.Unlock()
	if held || staged {
		return nil
	}
	if !there {
		return fmt.Errorf("provider %s has no piece %s to pull", from.Provider, pieceCID)
	}
	if err := sleep(ctx, 3*time.Second); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stage(ds.Provider, pieceCID)
	return nil
}

func (s *simStore) Commit(ctx context.Context, ds store.DataSet, pieceCIDs []string, _ map[string]string) ([]string, error) {
	s.mu.Lock()
	adds := false
	for _, c := range pieceCIDs {
		if _, held := s.st.Pieces[ds.ID][c]; !held {
			adds = true
		}
	}
	s.mu.Unlock()
	if adds {
		if err := sleep(ctx, simChain); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Pieces[ds.ID]
	ids := make([]string, len(pieceCIDs))
	for i, c := range pieceCIDs {
		if id, held := p[c]; held {
			ids[i] = id
			continue
		}
		ids[i] = strconv.Itoa(len(p))
		p[c] = ids[i]
		delete(s.st.Staged[ds.Provider], c)
	}
	s.save()
	return ids, nil
}

// Fetch reads the piece back from the archives on this machine, since the
// simulated providers keep no bytes.
func (s *simStore) Fetch(ctx context.Context, ds store.DataSet, pieceCID string) (io.ReadCloser, error) {
	s.mu.Lock()
	_, held := s.st.Pieces[ds.ID][pieceCID]
	s.mu.Unlock()
	if !held {
		return nil, fmt.Errorf("data set %s has no piece %s", ds.ID, pieceCID)
	}
	home, err := collection.DefaultDir()
	if err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(home, "archives", "*", "pieces", pieceCID+".car"))
	if len(files) == 0 {
		return nil, fmt.Errorf("piece %s is in no archive on this machine", pieceCID)
	}
	f, err := os.Open(files[0])
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{&paced{ctx: ctx, r: f, rate: simFetch, start: time.Now()}, f}, nil
}

func (s *simStore) Status(_ context.Context, archive string) (store.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st store.Status
	for _, d := range s.st.Sets {
		if d.Metadata["ipfs2fil"] == archive {
			st.DataSets = append(st.DataSets, store.DataSetStatus{DataSet: d, Pieces: len(s.st.Pieces[d.ID]), LastProven: "12 minutes ago"})
		}
	}
	st.FundedDays = 182
	return st, nil
}
