package foc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/filecoin-project/go-address"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	pdpclient "github.com/strahe/synapse-go/pdp"

	"github.com/jbenet/ipfs2fil/internal/store"
)

// fake is an in-memory FOC: providers 1..n, data sets with ids from 100.
type fake struct {
	mu        sync.Mutex // Create runs concurrently
	providers []string
	sets      []dataSet
	pieces    map[string]map[string]string // data set -> PieceCID -> piece id
	data      map[string][]byte            // PieceCID -> bytes
	staged    map[string]bool              // provider/PieceCID, sent but not added
	uploads   int
	pulls     int
	commits   int
	reads     int // Pieces calls
	nextID    int
	prepared  string // the last Prepare's ids, fresh providers and sizes
	// beforeCreate, if set, runs once inside the first Create, as another
	// machine creating data sets at the same moment.
	beforeCreate func()
}

func newFake(providers int) *fake {
	f := &fake{pieces: map[string]map[string]string{}, data: map[string][]byte{}, nextID: 100}
	for i := 1; i <= providers; i++ {
		f.providers = append(f.providers, strconv.Itoa(i))
	}
	return f
}

func (f *fake) DataSets(context.Context) ([]dataSet, error) { return slices.Clone(f.sets), nil }

func (f *fake) Providers(_ context.Context, n int, exclude []string, _ map[string]string) ([]string, error) {
	var out []string
	for _, p := range f.providers {
		if len(out) < n && !slices.Contains(exclude, p) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fake) Create(_ context.Context, p string, meta map[string]string) (dataSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if hook := f.beforeCreate; hook != nil {
		f.beforeCreate = nil
		hook()
	}
	return f.create(p, meta), nil
}

// create adds a data set; the caller holds f.mu.
func (f *fake) create(p string, meta map[string]string) dataSet {
	d := dataSet{ID: strconv.Itoa(f.nextID), Provider: p, Metadata: meta}
	f.nextID++
	f.sets = append(f.sets, d)
	f.pieces[d.ID] = map[string]string{}
	return d
}

func (f *fake) Pieces(_ context.Context, id string) (map[string]string, error) {
	f.reads++
	return maps.Clone(f.pieces[id]), nil
}

func (f *fake) Stage(_ context.Context, ds dataSet, pieceCID string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.uploads++
	f.data[pieceCID] = b
	f.stage(ds.Provider, pieceCID)
	return nil
}

func (f *fake) stage(provider, pieceCID string) {
	if f.staged == nil {
		f.staged = map[string]bool{}
	}
	f.staged[provider+"/"+pieceCID] = true
}

func (f *fake) Pull(_ context.Context, ds, from dataSet, pieceCID string) error {
	if _, ok := f.pieces[from.ID][pieceCID]; !ok && !f.staged[from.Provider+"/"+pieceCID] {
		return fmt.Errorf("data set %s has no piece %s", from.ID, pieceCID)
	}
	f.pulls++
	f.stage(ds.Provider, pieceCID)
	return nil
}

func (f *fake) Await(_ context.Context, ds dataSet, pieceCID string) error {
	if !f.staged[ds.Provider+"/"+pieceCID] {
		return fmt.Errorf("provider %s never took in piece %s", ds.Provider, pieceCID)
	}
	return nil
}

func (f *fake) Remove(_ context.Context, ds dataSet, pieceIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range pieceIDs {
		for c, pid := range f.pieces[ds.ID] {
			if pid == id {
				delete(f.pieces[ds.ID], c)
			}
		}
	}
	return nil
}

func (f *fake) Commit(_ context.Context, ds dataSet, pieceCIDs []string, meta map[string]string) ([]string, error) {
	if meta[KeyIPFSRoot] == "" {
		return nil, fmt.Errorf("no %s", KeyIPFSRoot)
	}
	var gone []string
	for _, c := range pieceCIDs {
		if !f.staged[ds.Provider+"/"+c] {
			gone = append(gone, c)
		}
	}
	if gone != nil {
		return nil, &store.GoneError{Provider: ds.Provider, PieceCIDs: gone}
	}
	f.commits++
	ids := make([]string, len(pieceCIDs))
	for i, c := range pieceCIDs {
		ids[i] = strconv.Itoa(len(f.pieces[ds.ID]))
		f.pieces[ds.ID][c] = ids[i]
		delete(f.staged, ds.Provider+"/"+c)
	}
	return ids, nil
}

func (f *fake) Download(_ context.Context, ds dataSet, pieceCID string) (io.ReadCloser, error) {
	if _, ok := f.pieces[ds.ID][pieceCID]; !ok {
		return nil, fmt.Errorf("not found")
	}
	return io.NopCloser(bytes.NewReader(f.data[pieceCID])), nil
}

func (f *fake) Proofs(_ context.Context, id string) (int64, time.Time, error) {
	if id == "100" {
		return 42, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC), nil
	}
	return 0, time.Time{}, nil
}

func (f *fake) Prepare(_ context.Context, ids, fresh []string, _ map[string]string, sizes []int64) (*Funding, error) {
	f.prepared = fmt.Sprint(ids, fresh, sizes)
	fd := &Funding{Deposit: big.NewInt(int64(len(fresh)) * 1e18), Monthly: new(big.Int), Fees: new(big.Int), Wallet: big.NewInt(1e18)}
	if len(fresh) > 0 {
		fd.apply = func(context.Context) error { return nil }
	}
	return fd, nil
}

func (f *fake) Runway(context.Context) (int, *big.Int, error) {
	return 30, new(big.Int).Mul(big.NewInt(15), big.NewInt(1e17)), nil
}

func (f *fake) ProviderURL(_ context.Context, p string) (string, error) {
	return "https://sp" + p + ".example", nil
}

func TestFundedDays(t *testing.T) {
	usdfc := func(f float64) *big.Int {
		v, _ := new(big.Float).Mul(big.NewFloat(f), big.NewFloat(1e18)).Int(nil)
		return v
	}
	// 0.36 USDFC a month; a month is 30 days of 2880 epochs.
	rate := new(big.Int).Div(usdfc(0.36), big.NewInt(30*2880))
	for _, c := range []struct {
		funds, fixed float64
		want         int
	}{
		{1.8747, 0, 156}, // the live run: was reported as 0
		{1.8747, 0.18, 141},
		{0.1, 0.2, 0},
	} {
		if got := fundedDays(usdfc(c.funds), usdfc(c.fixed), rate); got != c.want {
			t.Errorf("fundedDays(%v, %v) = %d, want %d", c.funds, c.fixed, got, c.want)
		}
	}
	if got := fundedDays(usdfc(1), nil, new(big.Int)); got != -1 {
		t.Errorf("no charges: %d", got)
	}
}

func push(t *testing.T, s *Store, archive string, copies int, pieces map[string]string) []store.DataSet {
	t.Helper()
	ctx := context.Background()
	sets, err := s.DataSets(ctx, archive, copies)
	if err != nil {
		t.Fatal(err)
	}
	for _, ds := range sets {
		for pc, body := range pieces {
			if _, err := s.Upload(ctx, ds, pc, strings.NewReader(body), int64(len(body)), map[string]string{KeyIPFSRoot: archive}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return sets
}

func TestPushThenRerunUploadsNothing(t *testing.T) {
	f := newFake(5)
	pieces := map[string]string{"bafkpieceA": "aaa", "bafkpieceB": "bbb"}
	sets := push(t, newStore(f), "bafyarchive", 3, pieces)
	if len(sets) != 3 || f.uploads != 6 {
		t.Fatalf("got %d data sets, %d uploads; want 3, 6", len(sets), f.uploads)
	}
	providers := map[string]bool{}
	for _, d := range sets {
		providers[d.Provider] = true
		if _, ok := d.Metadata[KeyIPFSIndexing]; !ok || d.Metadata[KeyArchive] != "bafyarchive" {
			t.Errorf("data set %s metadata %v", d.ID, d.Metadata)
		}
	}
	if len(providers) != 3 {
		t.Errorf("copies share providers: %v", sets)
	}

	// A new run (fresh Store) finds the same data sets and skips every piece.
	again := push(t, newStore(f), "bafyarchive", 3, pieces)
	if f.uploads != 6 || len(f.sets) != 3 {
		t.Fatalf("re-run uploaded %d pieces and made %d data sets", f.uploads-6, len(f.sets)-3)
	}
	for i := range sets {
		if again[i].ID != sets[i].ID {
			t.Errorf("copy %d moved from data set %s to %s", i, sets[i].ID, again[i].ID)
		}
	}

	// One new piece goes to all three copies; Pieces is read once per data set.
	f.reads = 0
	pieces["bafkpieceC"] = "ccc"
	push(t, newStore(f), "bafyarchive", 3, pieces)
	if f.uploads != 9 || f.reads != 3 {
		t.Errorf("uploads %d (want 9), chain reads %d (want 3)", f.uploads, f.reads)
	}
}

func TestArchivesKeepSeparateDataSets(t *testing.T) {
	f := newFake(3)
	a := push(t, newStore(f), "bafyA", 2, map[string]string{"p": "x"})
	b := push(t, newStore(f), "bafyB", 2, map[string]string{"p": "x"})
	if a[0].ID == b[0].ID || len(f.sets) != 4 {
		t.Fatalf("archives share data sets: %v %v", a, b)
	}
}

// TestFirstPushRace: another machine creates the key's data sets while ours
// are landing. Both machines then push into the oldest ones.
func TestFirstPushRace(t *testing.T) {
	f := newFake(5)
	f.beforeCreate = func() {
		for _, p := range []string{"3", "4", "5"} {
			f.create(p, dataSetMeta("repos:default"))
		}
	}
	s := newStore(f)
	sets, err := s.DataSets(context.Background(), "repos:default", 3)
	if err != nil {
		t.Fatal(err)
	}
	if ids := sets[0].ID + " " + sets[1].ID + " " + sets[2].ID; ids != "100 101 102" {
		t.Fatalf("used %s, want the other machine's 100 101 102", ids)
	}
	if len(f.sets) != 6 {
		t.Fatalf("%d data sets", len(f.sets))
	}
	other, err := newStore(f).DataSets(context.Background(), "repos:default", 3)
	if err != nil || other[0].ID != "100" || other[2].ID != "102" {
		t.Fatalf("the other machine uses %v: %v", other, err)
	}
	st, err := s.Status(context.Background(), "repos:default")
	if err != nil {
		t.Fatal(err)
	}
	// One per provider: the oldest three are used; ours on providers 1 and 2
	// stay listed (a push with more copies would use them), and ours on
	// provider 3 is a second one there.
	if len(st.DataSets) != 5 || len(st.Unused) != 1 || st.Unused[0].Provider != "3" {
		t.Errorf("status: %d data sets, unused %v", len(st.DataSets), st.Unused)
	}
}

func TestMoreCopiesAddsProviders(t *testing.T) {
	f := newFake(3)
	one := push(t, newStore(f), "bafyA", 1, map[string]string{"p": "x"})
	three := push(t, newStore(f), "bafyA", 3, map[string]string{"p": "x"})
	if three[0].ID != one[0].ID || len(f.sets) != 3 || f.uploads != 3 {
		t.Fatalf("got %v, %d data sets, %d uploads", three, len(f.sets), f.uploads)
	}
}

func TestPlanPricesOnlyMissingPieces(t *testing.T) {
	f := newFake(3)
	push(t, newStore(f), "bafyA", 3, map[string]string{"p1": "one", "p2": "two"})
	pieces := map[string]int64{"p1": 3, "p2": 300}

	// Every copy is full: nothing to price.
	if _, err := newStore(f).Plan(context.Background(), "bafyA", 3, pieces); err != nil || f.prepared != "[] [] []" {
		t.Fatalf("full copies priced as %s (%v)", f.prepared, err)
	}
	// One copy lost p2 (say its add failed): only that data set, only p2.
	delete(f.pieces["101"], "p2")
	if _, err := newStore(f).Plan(context.Background(), "bafyA", 3, pieces); err != nil || f.prepared != "[101] [] [300]" {
		t.Fatalf("one short copy priced as %s (%v)", f.prepared, err)
	}
	// A new copy needs everything.
	if _, err := newStore(f).Plan(context.Background(), "bafyA", 4, pieces); err == nil {
		t.Fatal("4 copies on 3 providers")
	}
	f.providers = append(f.providers, "4")
	if _, err := newStore(f).Plan(context.Background(), "bafyA", 4, pieces); err != nil || f.prepared != "[101] [4] [3 300]" {
		t.Fatalf("new copy priced as %s (%v)", f.prepared, err)
	}
}

func TestTopUp(t *testing.T) {
	for _, c := range []struct{ need, wallet, want int64 }{
		{0, 1e18, 0},
		{12, 1e18, 1e16},   // dust rounds up to 0.01 USDFC
		{12, 1e15, 12},     // unless the wallet lacks it
		{2e16, 1e18, 2e16}, // real shortfalls go as they are
	} {
		if got := topUp(big.NewInt(c.need), big.NewInt(c.wallet)); got.Int64() != c.want {
			t.Errorf("topUp(%d, %d) = %s, want %d", c.need, c.wallet, got, c.want)
		}
	}
}

func TestRetryCommit(t *testing.T) {
	ctx := context.Background()
	delays := []time.Duration{0, 0, 0}
	notFound := errors.New(`HTTP 400: failed to validate subPieces: subPiece CID baga... not found`)
	calls := 0
	err := retryCommit(ctx, delays, func() (bool, error) {
		if calls++; calls < 3 {
			return false, notFound
		}
		return true, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("not found twice: %v after %d calls", err, calls)
	}
	calls = 0
	if err := retryCommit(ctx, delays, func() (bool, error) { calls++; return false, notFound }); err == nil || calls != 4 {
		t.Fatalf("always not found: %v after %d calls", err, calls)
	}
	// Other errors, and anything after the add was submitted, are not retried.
	for _, submitted := range []bool{false, true} {
		calls = 0
		e := notFound
		if !submitted {
			e = errors.New("insufficient funds")
		}
		if err := retryCommit(ctx, delays, func() (bool, error) { calls++; return submitted, e }); err == nil || calls != 1 {
			t.Errorf("submitted=%v: %v after %d calls", submitted, err, calls)
		}
	}
}

func TestTooFewProviders(t *testing.T) {
	_, err := newStore(newFake(2)).DataSets(context.Background(), "bafyA", 3)
	if err == nil || !strings.Contains(err.Error(), "--copies 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlanUsesChosenProviders(t *testing.T) {
	f := newFake(5)
	s := newStore(f)
	ctx := context.Background()
	fd, err := s.Plan(ctx, "bafyA", 3, map[string]int64{"p": 1000})
	if err != nil {
		t.Fatal(err)
	}
	if fd.Ready() || USDFC(fd.Deposit) != "3" {
		t.Fatalf("funding %+v", fd)
	}
	if err := fd.Apply(ctx); err == nil || !strings.Contains(err.Error(), "wallet holds 1 USDFC") {
		t.Fatalf("Apply with too little USDFC: %v", err)
	}
	f.providers = []string{"5", "4", "3", "2", "1"} // selection would change now
	sets, err := s.DataSets(ctx, "bafyA", 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range sets {
		got = append(got, d.Provider)
	}
	slices.Sort(got)
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Errorf("copies on providers %v, want the planned [1 2 3]", got)
	}
	fd, err = s.Plan(ctx, "bafyA", 3, map[string]int64{"p": 1000})
	if err != nil || !fd.Ready() {
		t.Fatalf("existing data sets still need funding: %+v %v", fd, err)
	}
}

func TestStatusAndFetch(t *testing.T) {
	f := newFake(3)
	sets := push(t, newStore(f), "bafyA", 2, map[string]string{"p1": "one", "p2": "two"})
	push(t, newStore(f), "bafyB", 1, map[string]string{"p3": "three"})
	st, err := newStore(f).Status(context.Background(), "bafyA")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.DataSets) != 2 || st.DataSets[0].Pieces != 2 || st.FundedDays != 30 {
		t.Fatalf("status %+v", st)
	}
	// The reserve is the whole account's, so it counts bafyB's data set too.
	if st.Reserve != "1.5" || st.ReserveDataSets != 3 {
		t.Fatalf("status %+v", st)
	}
	if got := st.DataSets[0].LastProven; got != "epoch 42 (2026-10-24 12:00 UTC)" {
		t.Errorf("last proven %q", got)
	}
	if got := st.DataSets[0].URL; !strings.HasPrefix(got, "https://sp") {
		t.Errorf("URL %q", got)
	}
	if got := st.DataSets[1].LastProven; got != "not yet" {
		t.Errorf("last proven %q", got)
	}
	r, err := newStore(f).Fetch(context.Background(), sets[1], "p2")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r); string(b) != "two" {
		t.Errorf("fetched %q", b)
	}
}

func TestUSDFC(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "1000000000000000000": "1", "1250000000000000000": "1.25", "120000000000000": "0.0001", "12345678901234": "0.000012", "1": "0.000000000000000001"} {
		v, _ := new(big.Int).SetString(in, 10)
		if got := USDFC(v); got != want {
			t.Errorf("USDFC(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	hexKey := hex.EncodeToString(raw)
	lotus := func(typ string) string {
		return hex.EncodeToString(fmt.Appendf(nil, `{"Type":%q,"PrivateKey":%q}`, typ, base64.StdEncoding.EncodeToString(raw)))
	}
	for _, s := range []string{hexKey, "0x" + hexKey, hexKey + "\n", lotus("secp256k1")} {
		if _, err := parseKey(s); err != nil {
			t.Errorf("parseKey(%.10s…): %v", s, err)
		}
	}
	for s, want := range map[string]string{
		lotus("bls"): "BLS keys are not supported",
		"zz":         "not a hex private key",
		hexKey[:40]:  "32 bytes",
	} {
		_, err := parseKey(s)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseKey: %v, want %q", err, want)
		}
		if err != nil && strings.Contains(err.Error(), hexKey[:16]) {
			t.Errorf("error leaks key material: %v", err)
		}
	}
	t.Setenv(EnvKey, "")
	t.Setenv(EnvKeyFile, "")
	t.Setenv(EnvHome, t.TempDir())
	if _, err := LoadKey(""); err != errNoKey {
		t.Errorf("LoadKey with nothing set: %v", err)
	}
}

// A key 'wallet new' makes is private to you, never replaces a file, and is
// what LoadKey finds when nothing else is set.
func TestNewKeyFile(t *testing.T) {
	t.Setenv(EnvKey, "")
	t.Setenv(EnvKeyFile, "")
	t.Setenv(EnvHome, filepath.Join(t.TempDir(), "home"))
	p, err := DefaultKeyFile()
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewKeyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("key file: %v %v", st.Mode(), err)
	}
	if _, err := NewKeyFile(p); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second NewKeyFile: %v", err)
	}
	got, err := LoadKey("")
	if err != nil || !got.Equal(key) {
		t.Fatalf("LoadKey found %v, %v", got, err)
	}
	t.Setenv(EnvKey, hex.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if got, err := LoadKey(""); err != nil || got.Equal(key) {
		t.Errorf("$%s should win over the default file: %v", EnvKey, err)
	}
}

func TestFilecoinAddress(t *testing.T) {
	a := common.HexToAddress("0xd8F3b19a517A40D673B5a4389d64D179FB9531EB")
	f, err := FilecoinAddress(a, false)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := address.NewFromString(f)
	if err != nil || !strings.HasPrefix(f, "f410f") || !bytes.Equal(parsed.Payload()[1:], a.Bytes()) {
		t.Errorf("mainnet: %s %v", f, err)
	}
	if tf, _ := FilecoinAddress(a, true); tf != "t"+f[1:] {
		t.Errorf("calibration: %s", tf)
	}
}

// The PDPVerifier ABI decodes into the types Pieces and Proofs assert.
func TestPDPVerifierABI(t *testing.T) {
	a, err := abi.JSON(strings.NewReader(pdpVerifierABI))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := mh.Sum([]byte("x"), mh.SHA2_256, -1)
	c := cid.NewCidV1(cid.Raw, h)
	type cids = struct {
		Data []byte `json:"data"`
	}
	out, err := a.Methods["getActivePieces"].Outputs.Pack([]cids{{Data: c.Bytes()}}, []*big.Int{big.NewInt(9)}, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.Unpack("getActivePieces", out)
	if err != nil {
		t.Fatal(err)
	}
	pieces := v[0].([]struct {
		Data []byte `json:"data"`
	})
	if got, _ := cid.Cast(pieces[0].Data); got != c || v[1].([]*big.Int)[0].Int64() != 9 || v[2].(bool) {
		t.Fatalf("decoded %v", v)
	}
	for _, m := range []string{"getDataSetLastProvenEpoch", "provingActivationEpoch"} {
		if _, err := a.Pack(m, big.NewInt(1)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProven(t *testing.T) {
	for _, c := range []struct {
		last, activation int64
		want             bool
	}{
		{0, 0, false},
		{4187356, 0, false},       // pieces added, proving not started
		{4187356, 4187356, false}, // proving started, nothing proven yet
		{4187500, 4187356, true},
	} {
		if got := proven(big.NewInt(c.last), big.NewInt(c.activation)); got != c.want {
			t.Errorf("proven(%d, %d) = %v", c.last, c.activation, got)
		}
	}
}

func TestReadConfig(t *testing.T) {
	t.Setenv(EnvKey, "")
	t.Setenv(EnvKeyFile, "")
	t.Setenv(EnvAddress, "")
	if _, err := ReadConfig(CalibrationRPC, "", ""); err == nil || !strings.Contains(err.Error(), "--address") {
		t.Errorf("no key or address: %v", err)
	}
	t.Setenv(EnvAddress, "0xd8F3b19a517A40D673B5a4389d64D179FB9531EB")
	cfg, err := ReadConfig(CalibrationRPC, "", "")
	if err != nil || cfg.Key != nil || cfg.Address == "" {
		t.Errorf("address only: %+v %v", cfg, err)
	}
	t.Setenv(EnvKey, hex.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if cfg, err := ReadConfig(CalibrationRPC, "", ""); err != nil || cfg.Key == nil {
		t.Errorf("key wins: %+v %v", cfg, err)
	}
	if _, _, err := Open(context.Background(), Config{RPC: CalibrationRPC, Address: "f1abc"}); err == nil || !strings.Contains(err.Error(), "0x address") {
		t.Errorf("bad address: %v", err)
	}
}

func TestPullCopiesBetweenProviders(t *testing.T) {
	f := newFake(2)
	s := newStore(f)
	ctx := context.Background()
	sets, err := s.DataSets(ctx, "bafyA", 2)
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{KeyIPFSRoot: "bafyA"}
	// The second provider pulls the piece while the first only has it
	// staged, then each adds it.
	if err := s.Stage(ctx, sets[0], "p", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Pull(ctx, sets[1], sets[0], "p"); err != nil || f.pulls != 1 || f.uploads != 1 {
		t.Fatalf("pull: %v; %d pulls, %d uploads", err, f.pulls, f.uploads)
	}
	for _, ds := range sets {
		if ids, err := s.Commit(ctx, ds, []string{"p"}, meta); err != nil || ids[0] == "" {
			t.Fatalf("commit on %s: %v, %v", ds.Provider, ids, err)
		}
	}
	// Held already: no second pull, and no second add.
	if err := s.Pull(ctx, sets[1], sets[0], "p"); err != nil || f.pulls != 1 {
		t.Fatalf("re-pull: %v; %d pulls", err, f.pulls)
	}
	if _, err := s.Commit(ctx, sets[1], []string{"p"}, meta); err != nil || f.commits != 2 {
		t.Fatalf("re-commit: %v; %d commits", err, f.commits)
	}
	if err := s.Pull(ctx, sets[0], sets[1], "q"); err == nil || !strings.Contains(err.Error(), "fetching piece q from provider") {
		t.Fatalf("pull of a missing piece: %v", err)
	}
}

func TestCommitReportsDroppedPieces(t *testing.T) {
	// A piece staged, then dropped by the provider, comes back as a
	// GoneError through the Store's wrapping, and nothing is added.
	f := newFake(1)
	s := newStore(f)
	ctx := context.Background()
	sets, err := s.DataSets(ctx, "bafyA", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"p", "q"} {
		if err := s.Stage(ctx, sets[0], c, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	delete(f.staged, "1/q")
	_, err = s.Commit(ctx, sets[0], []string{"p", "q"}, map[string]string{KeyIPFSRoot: "bafyA"})
	var gone *store.GoneError
	if !errors.As(err, &gone) || fmt.Sprint(gone.PieceCIDs) != "[q]" || f.commits != 0 {
		t.Fatalf("got %v (%d commits), want q gone", err, f.commits)
	}
}

func TestUploadErrorsLeaveOutSessionID(t *testing.T) {
	inner := &pdpclient.HTTPError{Method: "PUT", URL: "https://sp.example/pdp/piece/uploads/1051470a-8822-437a-9b42-6f438dec5dc5", StatusCode: 500, Body: "boom"}
	err := error(sessionless{fmt.Errorf("upload: %w", inner)})
	if strings.Contains(err.Error(), "1051470a") || !strings.Contains(err.Error(), "/pdp/piece/uploads") {
		t.Fatalf("error text %q", err)
	}
	var he *pdpclient.HTTPError
	if !errors.As(err, &he) || he.StatusCode != 500 {
		t.Fatal("lost the HTTP error")
	}
}
