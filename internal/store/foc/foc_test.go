package foc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/store"
)

// fake is an in-memory FOC: providers 1..n, data sets with ids from 100.
type fake struct {
	providers []string
	sets      []dataSet
	pieces    map[string]map[string]string // data set -> PieceCID -> piece id
	data      map[string][]byte            // PieceCID -> bytes
	uploads   int
	reads     int // Pieces calls
	nextID    int
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
	d := dataSet{ID: strconv.Itoa(f.nextID), Provider: p, Metadata: meta}
	f.nextID++
	f.sets = append(f.sets, d)
	f.pieces[d.ID] = map[string]string{}
	return d, nil
}

func (f *fake) Pieces(_ context.Context, id string) (map[string]string, error) {
	f.reads++
	return maps.Clone(f.pieces[id]), nil
}

func (f *fake) Upload(_ context.Context, ds dataSet, pieceCID string, r io.Reader, meta map[string]string) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if meta[KeyIPFSRoot] == "" {
		return "", fmt.Errorf("no %s", KeyIPFSRoot)
	}
	f.uploads++
	f.data[pieceCID] = b
	id := strconv.Itoa(len(f.pieces[ds.ID]))
	f.pieces[ds.ID][pieceCID] = id
	return id, nil
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
	fd := &Funding{Deposit: big.NewInt(int64(len(fresh)) * 1e18), Monthly: new(big.Int), Fees: new(big.Int), Wallet: big.NewInt(1e18)}
	if len(fresh) > 0 {
		fd.apply = func(context.Context) error { return nil }
	}
	return fd, nil
}

func (f *fake) RunwayDays(context.Context) (int, error) { return 30, nil }

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

func TestMoreCopiesAddsProviders(t *testing.T) {
	f := newFake(3)
	one := push(t, newStore(f), "bafyA", 1, map[string]string{"p": "x"})
	three := push(t, newStore(f), "bafyA", 3, map[string]string{"p": "x"})
	if three[0].ID != one[0].ID || len(f.sets) != 3 || f.uploads != 3 {
		t.Fatalf("got %v, %d data sets, %d uploads", three, len(f.sets), f.uploads)
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
	fd, err := s.Plan(ctx, "bafyA", 3, []int64{1000})
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
	for i, want := range []string{"1", "2", "3"} {
		if sets[i].Provider != want {
			t.Errorf("copy %d on provider %s, want the planned %s", i, sets[i].Provider, want)
		}
	}
	fd, err = s.Plan(ctx, "bafyA", 3, []int64{1000})
	if err != nil || !fd.Ready() {
		t.Fatalf("existing data sets still need funding: %+v %v", fd, err)
	}
}

func TestStatusAndFetch(t *testing.T) {
	f := newFake(3)
	sets := push(t, newStore(f), "bafyA", 2, map[string]string{"p1": "one", "p2": "two"})
	st, err := newStore(f).Status(context.Background(), "bafyA")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.DataSets) != 2 || st.DataSets[0].Pieces != 2 || st.FundedDays != 30 {
		t.Fatalf("status %+v", st)
	}
	if got := st.DataSets[0].LastProven; got != "epoch 42 (2026-10-24 12:00 UTC)" {
		t.Errorf("last proven %q", got)
	}
	if got := st.DataSets[1].LastProven; got != "never" {
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
	for in, want := range map[string]string{"0": "0", "1000000000000000000": "1", "1250000000000000000": "1.25", "120000000000000": "0.0001"} {
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
	if _, err := LoadKey(""); err != errNoKey {
		t.Errorf("LoadKey with nothing set: %v", err)
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
	if _, err := a.Pack("getDataSetLastProvenEpoch", big.NewInt(1)); err != nil {
		t.Fatal(err)
	}
}
