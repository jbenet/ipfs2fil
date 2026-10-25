package foc

import (
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
	"github.com/strahe/synapse-go/storage"
	"github.com/strahe/synapse-go/types"
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

func (b *synapseBackend) Upload(ctx context.Context, ds dataSet, pieceCID string, r io.Reader, meta map[string]string) (string, error) {
	if b.readOnly {
		return "", errReadOnly
	}
	pc, err := cid.Decode(pieceCID)
	if err != nil {
		return "", fmt.Errorf("bad PieceCID: %w", err)
	}
	dsc, err := b.dataSetContext(ctx, ds.ID)
	if err != nil {
		return "", err
	}
	res, err := dsc.Upload(ctx, r, &storage.ContextUploadOptions{PieceCID: pc, PieceMetadata: meta})
	if err != nil {
		return "", err
	}
	if len(res.Copies) == 0 {
		for _, f := range res.FailedAttempts {
			err = errors.Join(err, f.Err)
		}
		return "", fmt.Errorf("not committed: %w", err)
	}
	return res.Copies[0].PieceID.String(), nil
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

// pdpVerifierABI is the part of PDPVerifier that synapse-go reads only
// internally.
const pdpVerifierABI = `[
{"type":"function","name":"getActivePieces","stateMutability":"view",
 "inputs":[{"name":"setId","type":"uint256"},{"name":"offset","type":"uint256"},{"name":"limit","type":"uint256"}],
 "outputs":[{"name":"pieces","type":"tuple[]","components":[{"name":"data","type":"bytes"}]},{"name":"pieceIds","type":"uint256[]"},{"name":"hasMore","type":"bool"}]},
{"type":"function","name":"getDataSetLastProvenEpoch","stateMutability":"view",
 "inputs":[{"name":"setId","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]}
]`

func (b *synapseBackend) call(ctx context.Context, method string, args ...any) ([]any, error) {
	data, err := b.pdp.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	to := b.c.ResolvedAddresses().PDPVerifier
	out, err := b.ec.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("PDPVerifier.%s: %w", method, err)
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
	return e.Int64(), chain.EpochToTime(b.c.Chain(), e), nil
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
		f.Deposit, f.Approve = t.DepositAmount, t.IncludesApproval
		f.apply = func(ctx context.Context) error {
			_, err := t.Execute(ctx, payments.WithWait(10*time.Minute))
			return err
		}
		wallet, err := b.c.Payments().WalletBalance(ctx, b.c.ResolvedAddresses().USDFC, b.c.Address())
		if err != nil {
			return nil, fmt.Errorf("reading the wallet's USDFC: %w", err)
		}
		f.Wallet = wallet
	}
	return f, nil
}

func (b *synapseBackend) RunwayDays(ctx context.Context) (int, error) {
	s, err := b.c.Payments().AccountSummary(ctx, b.payer)
	if err != nil {
		return 0, err
	}
	return fundedDays(s.Funds, s.TotalFixedLockup, s.LockupRatePerEpoch), nil
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
