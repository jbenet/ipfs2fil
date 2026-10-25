// Package foc is the Filecoin Onchain Cloud store: PDP data sets run by FWSS
// providers and paid through Filecoin Pay, reached with synapse-go.
//
// Each archive gets one data set per copy, each on a different provider. A
// data set is tagged with the archive key and withIPFSIndexing, so re-runs find
// it again and providers announce its blocks to IPNI. Pieces carry
// ipfsRootCID. Before uploading, a piece is looked up in the data set's active
// pieces on chain and skipped if present, which is what makes push resumable.
package foc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/jbenet/ipfs2fil/internal/store"
)

// Service names this store in manifest.Copy.Service.
const Service = "foc-fwss"

// Metadata keys. FWSS limits keys to 32 bytes and values to 128.
const (
	KeyArchive      = "ipfs2fil"         // data set: the archive key
	KeyIPFSIndexing = "withIPFSIndexing" // data set: providers announce to IPNI
	KeyIPFSRoot     = "ipfsRootCID"      // piece: the archive root it belongs to
)

// backend is the part of FOC the store needs. synapse.go implements it with
// synapse-go; tests use a fake.
type backend interface {
	// DataSets lists the live data sets the wallet pays for.
	DataSets(ctx context.Context) ([]dataSet, error)
	// Providers picks up to n healthy approved providers, none in exclude.
	Providers(ctx context.Context, n int, exclude []string, meta map[string]string) ([]string, error)
	// Create makes an empty data set on provider.
	Create(ctx context.Context, provider string, meta map[string]string) (dataSet, error)
	// Pieces lists a data set's active pieces on chain: PieceCID -> piece id.
	Pieces(ctx context.Context, dataSetID string) (map[string]string, error)
	Upload(ctx context.Context, ds dataSet, pieceCID string, r io.Reader, meta map[string]string) (pieceID string, err error)
	Download(ctx context.Context, ds dataSet, pieceCID string) (io.ReadCloser, error)
	// Proofs reads when the data set was last proven, zero until the first proof.
	Proofs(ctx context.Context, dataSetID string) (lastEpoch int64, last time.Time, err error)
	// Prepare prices storing pieces of these sizes in every data set listed
	// and on every new provider, and says what funding is missing.
	Prepare(ctx context.Context, dataSetIDs, newProviders []string, meta map[string]string, sizes []int64) (*Funding, error)
	// Runway is how long the deposit pays for storage at today's rate, and
	// the fixed lockup it holds back as data set reserves,
	// counting funds locked up for the rails; -1 when nothing is charged.
	Runway(ctx context.Context) (days int, reserve *big.Int, err error)
	// ProviderURL is where a provider serves pieces.
	ProviderURL(ctx context.Context, provider string) (string, error)
}

type dataSet struct {
	ID       string
	Provider string
	URL      string
	Metadata map[string]string
}

func (d dataSet) public() store.DataSet {
	return store.DataSet{Provider: d.Provider, ID: d.ID, Metadata: d.Metadata, URL: d.URL}
}

func fromPublic(d store.DataSet) dataSet {
	return dataSet{ID: d.ID, Provider: d.Provider, URL: d.URL, Metadata: d.Metadata}
}

// Funding is what a push needs from Filecoin Pay before it can start.
type Funding struct {
	// Deposit is the USDFC (base units, 18 decimals) to move from the wallet
	// into Filecoin Pay. Zero when the deposit already covers the push.
	Deposit *big.Int
	// Approve is true when FWSS is not yet approved to charge the account.
	Approve bool
	// Monthly is the storage rate after the push; Fees the one-time charges.
	Monthly, Fees *big.Int
	// Wallet is the USDFC the wallet holds outside Filecoin Pay.
	Wallet *big.Int

	apply func(ctx context.Context) error
}

// Ready reports whether the push can start without a funding transaction.
func (f *Funding) Ready() bool { return f.apply == nil }

// Apply sends the deposit and approval in one transaction and waits for it.
func (f *Funding) Apply(ctx context.Context) error {
	if f.apply == nil {
		return nil
	}
	if f.Wallet != nil && f.Deposit != nil && f.Wallet.Cmp(f.Deposit) < 0 {
		return fmt.Errorf("the wallet holds %s USDFC but the deposit needs %s; add USDFC to the wallet and run again",
			USDFC(f.Wallet), USDFC(f.Deposit))
	}
	return f.apply(ctx)
}

// USDFC formats base units (18 decimals) as a decimal amount, e.g. "1.25".
// Amounts under 0.0001 keep two significant digits rather than show as 0.
func USDFC(v *big.Int) string {
	if v == nil {
		return "0"
	}
	r := new(big.Rat).SetFrac(v, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	prec := 4
	for ; prec < 18 && v.Sign() != 0 && new(big.Rat).Abs(r).Cmp(big.NewRat(1, pow10(prec))) < 0; prec++ {
	}
	if prec > 4 {
		prec = min(prec+1, 18)
	}
	s := r.FloatString(prec)
	for s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// Store implements store.Store on FOC.
type Store struct {
	b backend

	mu     sync.Mutex
	pieces map[string]map[string]string // data set id -> PieceCID -> piece id
	chosen map[string][]string          // archive -> providers chosen by Plan, not yet used
}

var _ store.Store = (*Store)(nil)

// Service names this store in manifest.Copy.Service.
func (*Store) Service() string { return Service }

func newStore(b backend) *Store {
	return &Store{b: b, pieces: map[string]map[string]string{}, chosen: map[string][]string{}}
}

func dataSetMeta(archive string) map[string]string {
	return map[string]string{KeyArchive: archive, KeyIPFSIndexing: ""}
}

// existing returns the archive's data sets, oldest first, one per provider.
func (s *Store) existing(ctx context.Context, archive string) ([]dataSet, error) {
	all, err := s.b.DataSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading your data sets: %w", err)
	}
	var out []dataSet
	seen := map[string]bool{}
	slices.SortFunc(all, func(a, b dataSet) int { return cmpID(a.ID, b.ID) })
	for _, d := range all {
		if d.Metadata[KeyArchive] != archive || seen[d.Provider] {
			continue
		}
		seen[d.Provider] = true
		out = append(out, d)
	}
	return out, nil
}

// Plan says what pushing these pieces (PieceCID to CAR bytes) as copies
// copies would cost and what funding is missing. Data sets that already hold
// every piece are not charged again, and the others are priced for the
// pieces they lack. It picks providers for any data sets that do not exist
// yet; DataSets then creates them there.
func (s *Store) Plan(ctx context.Context, archive string, copies int, pieces map[string]int64) (*Funding, error) {
	have, err := s.existing(ctx, archive)
	if err != nil {
		return nil, err
	}
	have = have[:min(len(have), copies)]
	var ids, used []string
	lacking := map[string]bool{}
	for _, d := range have {
		used = append(used, d.Provider)
		held, err := s.active(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		need := false
		for c := range pieces {
			if _, ok := held[c]; !ok {
				lacking[c], need = true, true
			}
		}
		if need {
			ids = append(ids, d.ID)
		}
	}
	var fresh []string
	if n := copies - len(have); n > 0 {
		if fresh, err = s.b.Providers(ctx, n, used, dataSetMeta(archive)); err != nil {
			return nil, fmt.Errorf("choosing %d storage providers: %w", n, err)
		}
		if len(fresh) < n {
			return nil, fmt.Errorf("asked for %d copies but only %d providers are available; try --copies %d",
				copies, len(have)+len(fresh), len(have)+len(fresh))
		}
	}
	// Prepare takes one list of sizes for every data set: new ones need all
	// pieces, and existing ones are priced for every piece any of them lacks.
	var sizes []int64
	for c, n := range pieces {
		if len(fresh) > 0 || lacking[c] {
			sizes = append(sizes, n)
		}
	}
	slices.Sort(sizes)
	f, err := s.b.Prepare(ctx, ids, fresh, dataSetMeta(archive), sizes)
	if err != nil {
		return nil, fmt.Errorf("pricing the upload: %w", err)
	}
	// Monthly covers the data sets priced; the full ones cost the same per
	// copy once the push is done.
	if n := len(ids) + len(fresh); n > 0 && n < copies && f.Monthly != nil {
		f.Monthly = new(big.Int).Div(new(big.Int).Mul(f.Monthly, big.NewInt(int64(copies))), big.NewInt(int64(n)))
	}
	s.mu.Lock()
	s.chosen[archive] = fresh
	s.mu.Unlock()
	return f, nil
}

func (s *Store) DataSets(ctx context.Context, archive string, copies int) ([]store.DataSet, error) {
	have, err := s.existing(ctx, archive)
	if err != nil {
		return nil, err
	}
	have = have[:min(len(have), copies)]
	used := map[string]bool{}
	var exclude []string
	for _, d := range have {
		used[d.Provider] = true
		exclude = append(exclude, d.Provider)
	}
	s.mu.Lock()
	var next []string
	for _, p := range s.chosen[archive] {
		if !used[p] {
			next = append(next, p)
		}
	}
	delete(s.chosen, archive)
	s.mu.Unlock()
	if n := copies - len(have) - len(next); n > 0 {
		more, err := s.b.Providers(ctx, n, append(exclude, next...), dataSetMeta(archive))
		if err != nil {
			return nil, fmt.Errorf("choosing %d storage providers: %w", n, err)
		}
		next = append(next, more...)
	}
	// Each creation waits for the provider to land it on chain, a few
	// minutes, so the data sets are created in parallel.
	next = next[:min(len(next), copies-len(have))]
	made := make([]dataSet, len(next))
	errs := make([]error, len(next))
	var wg sync.WaitGroup
	for i, p := range next {
		wg.Go(func() {
			made[i], errs[i] = s.b.Create(ctx, p, dataSetMeta(archive))
			if errs[i] != nil {
				errs[i] = fmt.Errorf("creating a data set on provider %s: %w", p, errs[i])
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, d := range made {
		s.pieces[d.ID] = map[string]string{}
	}
	s.mu.Unlock()
	slices.SortFunc(made, func(a, b dataSet) int { return cmpID(a.ID, b.ID) })
	have = append(have, made...)
	if len(have) < copies {
		return nil, fmt.Errorf("asked for %d copies but only %d providers are available; try --copies %d",
			copies, len(have), len(have))
	}
	out := make([]store.DataSet, len(have))
	for i, d := range have {
		out[i] = d.public()
	}
	return out, nil
}

// active returns the data set's pieces, read from chain once per run.
func (s *Store) active(ctx context.Context, id string) (map[string]string, error) {
	s.mu.Lock()
	p, ok := s.pieces[id]
	s.mu.Unlock()
	if ok {
		return p, nil
	}
	p, err := s.b.Pieces(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the pieces of data set %s: %w", id, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if q, ok := s.pieces[id]; ok {
		return q, nil
	}
	s.pieces[id] = p
	return p, nil
}

func (s *Store) Has(ctx context.Context, ds store.DataSet, pieceCID string) (bool, error) {
	_, ok, err := s.lookup(ctx, ds.ID, pieceCID)
	return ok, err
}

func (s *Store) lookup(ctx context.Context, id, pieceCID string) (string, bool, error) {
	p, err := s.active(ctx, id)
	if err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pid, ok := p[pieceCID]
	return pid, ok, nil
}

// Upload stores the piece unless the data set already holds it.
func (s *Store) Upload(ctx context.Context, ds store.DataSet, pieceCID string, car io.Reader, size int64, meta map[string]string) (store.PieceRef, error) {
	ref := store.PieceRef{DataSetID: ds.ID, PieceCID: pieceCID}
	pid, ok, err := s.lookup(ctx, ds.ID, pieceCID)
	if err != nil {
		return ref, err
	}
	if ok {
		ref.PieceID = pid
		return ref, nil
	}
	pid, err = s.b.Upload(ctx, fromPublic(ds), pieceCID, car, meta)
	if err != nil {
		return ref, fmt.Errorf("uploading piece %s to provider %s: %w", pieceCID, ds.Provider, err)
	}
	s.mu.Lock()
	s.pieces[ds.ID][pieceCID] = pid
	s.mu.Unlock()
	ref.PieceID = pid
	return ref, nil
}

func (s *Store) Fetch(ctx context.Context, ds store.DataSet, pieceCID string) (io.ReadCloser, error) {
	r, err := s.b.Download(ctx, fromPublic(ds), pieceCID)
	if err != nil {
		return nil, fmt.Errorf("fetching piece %s from provider %s: %w", pieceCID, ds.Provider, err)
	}
	return r, nil
}

func (s *Store) Status(ctx context.Context, archive string) (store.Status, error) {
	var st store.Status
	have, err := s.existing(ctx, archive)
	if err != nil {
		return st, err
	}
	for _, d := range have {
		p, err := s.active(ctx, d.ID)
		if err != nil {
			return st, err
		}
		epoch, at, err := s.b.Proofs(ctx, d.ID)
		if err != nil {
			return st, fmt.Errorf("reading proofs of data set %s: %w", d.ID, err)
		}
		if d.URL == "" {
			if d.URL, err = s.b.ProviderURL(ctx, d.Provider); err != nil {
				return st, fmt.Errorf("looking up provider %s: %w", d.Provider, err)
			}
		}
		last := "not yet"
		if epoch > 0 {
			last = fmt.Sprintf("epoch %d (%s)", epoch, at.UTC().Format("2006-01-02 15:04 UTC"))
		}
		st.DataSets = append(st.DataSets, store.DataSetStatus{DataSet: d.public(), Pieces: len(p), LastProven: last})
	}
	days, reserve, err := s.b.Runway(ctx)
	if err != nil {
		return st, fmt.Errorf("reading your Filecoin Pay account: %w", err)
	}
	all, err := s.b.DataSets(ctx)
	if err != nil {
		return st, err
	}
	st.FundedDays, st.Reserve, st.ReserveDataSets = days, USDFC(reserve), len(all)
	return st, nil
}

// cmpID orders decimal ids numerically.
func cmpID(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
}
