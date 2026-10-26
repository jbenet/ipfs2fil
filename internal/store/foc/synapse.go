package foc

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ipfs/go-cid"
	synapse "github.com/strahe/synapse-go"
	"github.com/strahe/synapse-go/chain"
	"github.com/strahe/synapse-go/payments"
	pdpclient "github.com/strahe/synapse-go/pdp"
	"github.com/strahe/synapse-go/storage"
	"github.com/strahe/synapse-go/types"

	"github.com/jbenet/ipfs2fil/internal/store"
)

// Public RPC endpoints.
const (
	MainnetRPC     = "https://api.node.glif.io/rpc/v1"
	CalibrationRPC = "https://api.calibration.node.glif.io/rpc/v1"
)

type Config struct {
	RPC string // e.g. CalibrationRPC; the network is read from it
	Key *ecdsa.PrivateKey
	// Address opens the store read-only for this wallet (0x...) when Key is
	// nil: Status, Has and Fetch work, and anything that writes fails.
	Address string
	// MaxBaseFee holds on-chain steps while the network base fee (attoFIL
	// per gas) is at or above it: 0 means DefaultMaxBaseFee, negative never
	// waits. OnGas, if set, hears when steps start and stop waiting.
	MaxBaseFee int64
	OnGas      func(baseFee *big.Int, paused bool)
}

var errReadOnly = errors.New("this needs the wallet key; the store was opened with an address only")

// Open connects to FOC as the wallet in cfg.Key, or read-only as cfg.Address.
// Close the store when done.
func Open(ctx context.Context, cfg Config) (*Store, io.Closer, error) {
	key, payer := cfg.Key, common.Address{}
	switch {
	case key != nil:
		payer = crypto.PubkeyToAddress(key.PublicKey)
	case cfg.Address != "":
		if !common.IsHexAddress(cfg.Address) {
			return nil, nil, fmt.Errorf("wallet address %q is not a 0x address", cfg.Address)
		}
		payer = common.HexToAddress(cfg.Address)
		// synapse-go needs a signer even for reads; this one never signs
		// anything that reaches the chain.
		var err error
		if key, err = crypto.GenerateKey(); err != nil {
			return nil, nil, err
		}
	default:
		return nil, nil, errNoKey
	}
	ec, err := ethclient.DialContext(ctx, cfg.RPC)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", cfg.RPC, err)
	}
	c, err := synapse.New(ctx,
		synapse.WithPrivateKey(key),
		synapse.WithEthClient(ec),
		synapse.WithSource("ipfs2fil"),
	)
	if err != nil {
		ec.Close()
		return nil, nil, fmt.Errorf("connecting to Filecoin Onchain Cloud via %s: %w", cfg.RPC, err)
	}
	pdp, err := abi.JSON(strings.NewReader(pdpVerifierABI))
	if err != nil {
		panic(err)
	}
	b := &synapseBackend{c: c, ec: ec, pdp: pdp, payer: payer, readOnly: cfg.Key == nil, dsc: map[string]*storage.DataSetContext{}}
	if cfg.MaxBaseFee >= 0 {
		b.gas = &gasGate{max: big.NewInt(cmp.Or(cfg.MaxBaseFee, DefaultMaxBaseFee)), every: time.Minute, report: cfg.OnGas,
			read: func(ctx context.Context) (*big.Int, error) {
				h, err := ec.HeaderByNumber(ctx, nil)
				if err != nil {
					return nil, err
				}
				return h.BaseFee, nil
			}}
	}
	return newStore(b), b, nil
}

// uploadRunway is the payment time a deposit adds beyond the lockup, so the
// account stays funded while a push runs.
const uploadRunway = chain.EpochsPerDay

type synapseBackend struct {
	c        *synapse.Client
	ec       *ethclient.Client
	pdp      abi.ABI
	payer    common.Address
	readOnly bool
	gas      *gasGate

	mu  sync.Mutex
	dsc map[string]*storage.DataSetContext
}

// Close waits for pending commits, then disconnects.
func (b *synapseBackend) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	err := b.c.Storage().Flush(ctx)
	err = errors.Join(err, b.c.Close())
	b.ec.Close()
	return err
}

func (b *synapseBackend) DataSets(ctx context.Context) ([]dataSet, error) {
	all, err := b.c.Storage().FindDataSets(ctx, &storage.FindDataSetsOptions{Payer: b.payer, OnlyManaged: true})
	if err != nil {
		return nil, err
	}
	var out []dataSet
	for _, d := range all {
		if !d.IsLive {
			continue
		}
		out = append(out, dataSet{ID: d.DataSetID.String(), Provider: d.ProviderID.String(), Metadata: d.Metadata})
	}
	return out, nil
}

func (b *synapseBackend) Providers(ctx context.Context, n int, exclude []string, meta map[string]string) ([]string, error) {
	if b.readOnly {
		return nil, errReadOnly
	}
	ex, err := bigInts(exclude)
	if err != nil {
		return nil, err
	}
	sel, err := b.c.Storage().SelectUploadContexts(ctx, storage.SelectUploadContextsOptions{
		Copies:             n,
		ExcludeProviderIDs: ex,
		DataSetMetadata:    meta,
		// Copies after the first sit next to existing data sets, so any
		// approved provider will do.
		AllowUnendorsedPrimary: len(exclude) > 0,
	})
	if err != nil && !errors.Is(err, storage.ErrInsufficientUploadContexts) {
		// Every eligible provider is excluded (already holds a copy):
		// report none left, so callers can say how many copies fit.
		if strings.Contains(err.Error(), "no eligible providers") || strings.Contains(err.Error(), "no remaining providers") {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, c := range sel.Contexts {
		out = append(out, c.ProviderID().String())
	}
	return out, nil
}

func (b *synapseBackend) providerContext(ctx context.Context, provider string, meta map[string]string) (*storage.ProviderContext, error) {
	id, err := types.ParseBigInt(provider)
	if err != nil {
		return nil, err
	}
	return b.c.Storage().NewProviderContext(ctx, id, storage.NewProviderContextOptions{DataSetMetadata: meta})
}

func (b *synapseBackend) Create(ctx context.Context, provider string, meta map[string]string) (dataSet, error) {
	if b.readOnly {
		return dataSet{}, errReadOnly
	}
	pc, err := b.providerContext(ctx, provider, meta)
	if err != nil {
		return dataSet{}, err
	}
	if err := b.gas.wait(ctx); err != nil {
		return dataSet{}, err
	}
	res, err := pc.CreateDataSet(ctx, nil)
	if err != nil {
		return dataSet{}, err
	}
	dsc, err := pc.ForDataSet(res.DataSet)
	if err != nil {
		return dataSet{}, err
	}
	id := res.DataSet.DataSetID().String()
	b.mu.Lock()
	b.dsc[id] = dsc
	b.mu.Unlock()
	return dataSet{ID: id, Provider: provider, URL: pc.ServiceURL(), Metadata: meta}, nil
}

func (b *synapseBackend) dataSetContext(ctx context.Context, id string) (*storage.DataSetContext, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if c, ok := b.dsc[id]; ok {
		return c, nil
	}
	n, err := types.ParseBigInt(id)
	if err != nil {
		return nil, err
	}
	c, err := b.c.Storage().NewDataSetContext(ctx, n, storage.NewDataSetContextOptions{})
	if err != nil {
		return nil, err
	}
	b.dsc[id] = c
	return c, nil
}

func (b *synapseBackend) Stage(ctx context.Context, ds dataSet, pieceCID string, r io.Reader) error {
	if b.readOnly {
		return errReadOnly
	}
	pc, err := cid.Decode(pieceCID)
	if err != nil {
		return fmt.Errorf("bad PieceCID: %w", err)
	}
	dsc, err := b.dataSetContext(ctx, ds.ID)
	if err != nil {
		return err
	}
	// A run that sent the piece but stopped before it was added leaves it
	// parked with the provider for a while: it need not be sent again.
	if ok, err := parked(ctx, dsc.ServiceURL(), pc); err == nil && ok {
		return nil
	}
	_, err = dsc.Store(ctx, r, &storage.StoreOptions{PieceCID: pc})
	return err
}

// parked reports whether the provider at serviceURL holds the piece, ready
// to add to a data set; err says when it could not tell.
var parked = func(ctx context.Context, serviceURL string, pc cid.Cid) (bool, error) {
	c, err := pdpclient.New(serviceURL)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = c.FindPiece(ctx, pc)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pdpclient.ErrPieceNotFound):
		return false, nil
	}
	return false, err
}

// Pull has ds's provider fetch the piece from from's provider, which serves
// it at /piece/<PieceCID> once it is parked there. The request carries a
// signed authorization to add the piece, as synapse-go's own multi-copy
// upload does; Commit signs its own for the pieces it adds together.
func (b *synapseBackend) Pull(ctx context.Context, ds, from dataSet, pieceCID string) error {
	if b.readOnly {
		return errReadOnly
	}
	pc, err := cid.Decode(pieceCID)
	if err != nil {
		return fmt.Errorf("bad PieceCID: %w", err)
	}
	src, err := b.dataSetContext(ctx, from.ID)
	if err != nil {
		return err
	}
	dsc, err := b.dataSetContext(ctx, ds.ID)
	if err != nil {
		return err
	}
	if ok, err := parked(ctx, dsc.ServiceURL(), pc); err == nil && ok {
		return nil
	}
	extra, err := dsc.PresignForCommit(ctx, []storage.PieceInput{{PieceCID: pc}})
	if err != nil {
		return fmt.Errorf("signing the pull: %w", err)
	}
	res, err := dsc.Pull(ctx, storage.PullRequest{Pieces: []cid.Cid{pc}, From: src.PieceURL, ExtraData: extra})
	if err != nil {
		return err
	}
	if res == nil {
		return errors.New("the provider reported no pull status")
	}
	if res.Status != storage.PullStatusComplete {
		return fmt.Errorf("the pull ended %s", res.Status)
	}
	return nil
}

// Commit adds pieces the provider holds to ds, once the base fee allows. A
// piece can wait a long time for that, and providers drop parked pieces
// about two hours after they arrive, so each is checked first and the
// dropped ones are reported in a *store.GoneError for the caller to send
// again.
func (b *synapseBackend) Commit(ctx context.Context, ds dataSet, pieceCIDs []string, meta map[string]string) ([]string, error) {
	if b.readOnly {
		return nil, errReadOnly
	}
	pieces := make([]storage.PieceInput, len(pieceCIDs))
	for i, c := range pieceCIDs {
		pc, err := cid.Decode(c)
		if err != nil {
			return nil, fmt.Errorf("bad PieceCID: %w", err)
		}
		pieces[i] = storage.PieceInput{PieceCID: pc, PieceMetadata: meta}
	}
	dsc, err := b.dataSetContext(ctx, ds.ID)
	if err != nil {
		return nil, err
	}
	if err := b.gas.wait(ctx); err != nil {
		return nil, err
	}
	if err := b.gone(ctx, dsc.ServiceURL(), ds, pieces); err != nil {
		return nil, err
	}
	var res *storage.CommitResult
	err = retryCommit(ctx, commitDelays, func() (submitted bool, err error) {
		res, err = dsc.Commit(ctx, storage.CommitRequest{
			Pieces:      pieces,
			OnSubmitted: func(storage.CommitSubmission) { submitted = true },
		})
		return submitted, err
	})
	if err != nil {
		if ctx.Err() == nil && strings.Contains(err.Error(), "not found") {
			if gone := b.gone(ctx, dsc.ServiceURL(), ds, pieces); gone != nil {
				return nil, gone
			}
		}
		return nil, err
	}
	ids := make([]string, len(res.PieceIDs))
	for i, id := range res.PieceIDs {
		ids[i] = id.String()
	}
	return ids, nil
}

// gone returns a *store.GoneError naming the pieces the provider says it no
// longer holds, or nil. Pieces it cannot check are assumed held: the add then
// fails or succeeds on its own.
func (b *synapseBackend) gone(ctx context.Context, serviceURL string, ds dataSet, pieces []storage.PieceInput) error {
	var out []string
	for _, p := range pieces {
		if ok, err := parked(ctx, serviceURL, p.PieceCID); err == nil && !ok {
			out = append(out, p.PieceCID.String())
		}
	}
	if out == nil {
		return nil
	}
	return &store.GoneError{Provider: ds.Provider, PieceCIDs: out}
}

// commitDelays are the waits between attempts to add a just-stored piece. A
// provider can reject the add for a minute after the upload, with "subPiece
// CID ... not found", until it has indexed the piece.
var commitDelays = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}

// retryCommit calls commit until it succeeds, retrying only while the
// provider has not yet seen the piece. A commit that reached the chain is
// never sent again.
func retryCommit(ctx context.Context, delays []time.Duration, commit func() (submitted bool, err error)) error {
	for i := 0; ; i++ {
		submitted, err := commit()
		if err == nil || submitted || i == len(delays) || !strings.Contains(err.Error(), "not found") {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delays[i]):
		}
	}
}

func (b *synapseBackend) Download(ctx context.Context, ds dataSet, pieceCID string) (io.ReadCloser, error) {
	pc, err := cid.Decode(pieceCID)
	if err != nil {
		return nil, fmt.Errorf("bad PieceCID: %w", err)
	}
	if b.readOnly {
		// Data set downloads check that the client owns the data set, so
		// read-only fetches go straight to the provider.
		prov, err := types.ParseBigInt(ds.Provider)
		if err != nil {
			return nil, err
		}
		pc2, err := b.c.Storage().NewProviderContext(ctx, prov, storage.NewProviderContextOptions{})
		if err != nil {
			return nil, err
		}
		return b.c.Storage().Download(ctx, pc, &storage.DownloadOptions{URL: pc2.PieceURL(pc)})
	}
	id, err := types.ParseBigInt(ds.ID)
	if err != nil {
		return nil, err
	}
	return b.c.Storage().Download(ctx, pc, &storage.DownloadOptions{DataSetID: &id})
}

// pdpVerifierABI is the part of PDPVerifier and the FWSS state view that
// synapse-go reads only internally.
const pdpVerifierABI = `[
{"type":"function","name":"getActivePieces","stateMutability":"view",
 "inputs":[{"name":"setId","type":"uint256"},{"name":"offset","type":"uint256"},{"name":"limit","type":"uint256"}],
 "outputs":[{"name":"pieces","type":"tuple[]","components":[{"name":"data","type":"bytes"}]},{"name":"pieceIds","type":"uint256[]"},{"name":"hasMore","type":"bool"}]},
{"type":"function","name":"getDataSetLastProvenEpoch","stateMutability":"view",
 "inputs":[{"name":"setId","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
{"type":"function","name":"provingActivationEpoch","stateMutability":"view",
 "inputs":[{"name":"dataSetId","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]}
]`

func (b *synapseBackend) call(ctx context.Context, method string, args ...any) ([]any, error) {
	data, err := b.pdp.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	to, name := b.c.ResolvedAddresses().PDPVerifier, "PDPVerifier"
	if method == "provingActivationEpoch" {
		to, name = b.c.ResolvedAddresses().ViewContract, "FWSS view"
	}
	out, err := b.ec.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("%s.%s: %w", name, method, err)
	}
	return b.pdp.Unpack(method, out)
}

func (b *synapseBackend) Pieces(ctx context.Context, dataSetID string) (map[string]string, error) {
	id, ok := new(big.Int).SetString(dataSetID, 10)
	if !ok {
		return nil, fmt.Errorf("bad data set id %q", dataSetID)
	}
	const page = 500
	out := map[string]string{}
	for off := int64(0); ; off += page {
		v, err := b.call(ctx, "getActivePieces", id, big.NewInt(off), big.NewInt(page))
		if err != nil {
			return nil, err
		}
		pieces := v[0].([]struct {
			Data []byte `json:"data"`
		})
		ids := v[1].([]*big.Int)
		for i, p := range pieces {
			c, err := cid.Cast(p.Data)
			if err != nil {
				return nil, fmt.Errorf("piece %s has a bad CID: %w", ids[i], err)
			}
			out[c.String()] = ids[i].String()
		}
		if !v[2].(bool) {
			return out, nil
		}
	}
}

func (b *synapseBackend) Proofs(ctx context.Context, dataSetID string) (int64, time.Time, error) {
	id, ok := new(big.Int).SetString(dataSetID, 10)
	if !ok {
		return 0, time.Time{}, fmt.Errorf("bad data set id %q", dataSetID)
	}
	v, err := b.call(ctx, "getDataSetLastProvenEpoch", id)
	if err != nil {
		return 0, time.Time{}, err
	}
	e := v[0].(*big.Int)
	if e.Sign() == 0 {
		return 0, time.Time{}, nil
	}
	// The first nextProvingPeriod sets the last proven epoch to the
	// activation epoch before anything is proven; a real proof comes later,
	// after the challenge window.
	v, err = b.call(ctx, "provingActivationEpoch", id)
	if err != nil {
		return 0, time.Time{}, err
	}
	if !proven(e, v[0].(*big.Int)) {
		return 0, time.Time{}, nil
	}
	return e.Int64(), chain.EpochToTime(b.c.Chain(), e), nil
}

// proven says whether last is a proof rather than proving's start.
func proven(last, activation *big.Int) bool {
	return last.Sign() > 0 && activation.Sign() > 0 && last.Cmp(activation) > 0
}

func (b *synapseBackend) Prepare(ctx context.Context, ids, fresh []string, meta map[string]string, sizes []int64) (*Funding, error) {
	if b.readOnly {
		return nil, errReadOnly
	}
	f := &Funding{Deposit: new(big.Int), Monthly: new(big.Int), Fees: new(big.Int)}
	if len(sizes) == 0 {
		return f, nil
	}
	var contexts []storage.StorageContext
	for _, id := range ids {
		c, err := b.dataSetContext(ctx, id)
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, c)
	}
	for _, p := range fresh {
		c, err := b.providerContext(ctx, p, meta)
		if err != nil {
			return nil, err
		}
		contexts = append(contexts, c)
	}
	us := make([]uint64, len(sizes))
	for i, s := range sizes {
		us[i] = uint64(s)
	}
	// Without extra runway the deposit covers the lockup exactly, and the rail
	// starts paying out when the first data set is created. Pieces added a few
	// minutes later then fail with "insufficient funds to cover lockup".
	prep, err := b.c.Storage().Prepare(ctx, &storage.PrepareOptions{
		PieceSizes:        us,
		Contexts:          contexts,
		ExtraRunwayEpochs: uploadRunway,
	})
	if err != nil {
		return nil, err
	}
	if prep.Costs != nil {
		f.Monthly, f.Fees = prep.Costs.RatePerMonth, prep.Costs.Fees.Total
	}
	if t := prep.Transaction; t != nil {
		wallet, err := b.c.Payments().WalletBalance(ctx, b.c.ResolvedAddresses().USDFC, b.c.Address())
		if err != nil {
			return nil, fmt.Errorf("reading the wallet's USDFC: %w", err)
		}
		f.Deposit, f.Approve, f.Wallet = t.DepositAmount, t.IncludesApproval, wallet
		f.apply = func(ctx context.Context) error {
			if err := b.gas.wait(ctx); err != nil {
				return err
			}
			_, err := t.Execute(ctx, payments.WithWait(10*time.Minute))
			return err
		}
		if d := topUp(t.DepositAmount, wallet); d.Cmp(t.DepositAmount) != 0 {
			opts := []payments.WriteOption{payments.WithWait(10 * time.Minute), payments.WithFundNeedsFwssApproval(t.IncludesApproval)}
			if t.IncludesApproval && prep.Costs != nil && prep.Costs.RequiredLockupPeriod != nil {
				opts = append(opts, payments.WithFundApprovalLockupPeriod(prep.Costs.RequiredLockupPeriod))
			}
			f.Deposit = d
			f.apply = func(ctx context.Context) error {
				if err := b.gas.wait(ctx); err != nil {
					return err
				}
				_, err := b.c.Payments().FundSync(ctx, d, opts...)
				return err
			}
		}
	}
	return f, nil
}

// minDeposit is the smallest deposit sent. The account drains every epoch, so
// a deposit sized to the exact shortfall leaves the next push short by a few
// epochs and sends another transaction for a fraction of a cent.
var minDeposit = big.NewInt(1e16) // 0.01 USDFC

// topUp is the deposit to send for a shortfall of need: at least minDeposit
// when the wallet holds it.
func topUp(need, wallet *big.Int) *big.Int {
	if need.Sign() <= 0 || need.Cmp(minDeposit) >= 0 || wallet == nil || wallet.Cmp(minDeposit) < 0 {
		return need
	}
	return new(big.Int).Set(minDeposit)
}

func (b *synapseBackend) Runway(ctx context.Context) (int, *big.Int, error) {
	s, err := b.c.Payments().AccountSummary(ctx, b.payer)
	if err != nil {
		return 0, nil, err
	}
	return fundedDays(s.Funds, s.TotalFixedLockup, s.LockupRatePerEpoch), s.TotalFixedLockup, nil
}

// fundedDays is how many days funds pay for at rate per epoch. Rate lockup
// is prepaid storage, so it counts; fixed lockup (the data sets' lifecycle
// reserve) does not. synapse-go's RunwayInEpochs leaves out all lockup, which
// reads as 0 days right after a deposit sized to the lockup.
func fundedDays(funds, fixed, rate *big.Int) int {
	if rate == nil || rate.Sign() == 0 {
		return -1
	}
	left := new(big.Int)
	if funds != nil {
		left.Set(funds)
	}
	if fixed != nil {
		left.Sub(left, fixed)
	}
	if left.Sign() <= 0 {
		return 0
	}
	days := left.Div(left, rate).Div(left, big.NewInt(chain.EpochsPerDay))
	if !days.IsInt64() || days.Int64() > 1<<31-1 {
		return 1<<31 - 1
	}
	return int(days.Int64())
}

func (b *synapseBackend) ProviderURL(ctx context.Context, provider string) (string, error) {
	pc, err := b.providerContext(ctx, provider, nil)
	if err != nil {
		return "", err
	}
	return pc.ServiceURL(), nil
}

func bigInts(ss []string) ([]types.BigInt, error) {
	out := make([]types.BigInt, len(ss))
	for i, s := range ss {
		v, err := types.ParseBigInt(s)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Balances is what a wallet holds: FIL for gas, and USDFC both in the wallet
// and deposited in Filecoin Pay, where storage is paid from.
type Balances struct {
	Address common.Address
	Testnet bool
	FIL     *big.Int // attoFIL
	USDFC   *big.Int // in the wallet, not yet deposited
	Deposit *big.Int // in Filecoin Pay
	// Available is the part of Deposit not locked up by data sets.
	Available *big.Int
}

// Balances reads the wallet's balances. Only the FOC store has them, so the
// method is on Store rather than in the backend interface.
func (s *Store) Balances(ctx context.Context) (Balances, error) {
	b, ok := s.b.(*synapseBackend)
	if !ok {
		return Balances{}, errors.New("this store has no wallet balances")
	}
	return b.balances(ctx)
}

func (b *synapseBackend) balances(ctx context.Context) (Balances, error) {
	out := Balances{Address: b.payer, Testnet: b.c.Chain() != chain.Mainnet}
	var err error
	if out.FIL, err = b.ec.BalanceAt(ctx, b.payer, nil); err != nil {
		return out, fmt.Errorf("reading the FIL balance: %w", err)
	}
	if out.USDFC, err = b.c.Payments().WalletBalance(ctx, b.c.ResolvedAddresses().USDFC, b.payer); err != nil {
		return out, fmt.Errorf("reading the USDFC balance: %w", err)
	}
	sum, err := b.c.Payments().AccountSummary(ctx, b.payer)
	if err != nil {
		return out, fmt.Errorf("reading your Filecoin Pay account: %w", err)
	}
	out.Deposit, out.Available = sum.Funds, sum.AvailableFunds
	return out, nil
}
