package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"

	"github.com/jbenet/ipfs2fil/internal/store/foc"
)

func init() {
	register(&command{
		name:    "wallet",
		summary: "Make a wallet key to fund, or show the wallet's balances",
		help: `
  ipfs2fil wallet new [--key-file <path>] [--network calibration]
  ipfs2fil wallet [--json]

'wallet new' makes a new wallet key on this machine and prints its address,
so you can send it FIL for gas and USDFC to pay for storage. The key goes to
--key-file, else $IPFS2FIL_KEY_FILE, else ~/.ipfs2fil/wallet.key (in
$IPFS2FIL_HOME if set), readable only by you, and the other commands find it
there by themselves. It never replaces an existing file. Back the file up:
it is the only copy, and whoever holds it can spend the wallet.

'wallet' prints the address of the wallet key (or --address) with its FIL,
its USDFC, and the USDFC deposited in Filecoin Pay, which storage is paid
from. 'push' deposits from the wallet's USDFC as needed.`,
		flags: func(fs *flag.FlagSet, o *opts) {
			jsonFlag(fs, o)
			addReadStoreFlags(fs)
		},
		run: runWallet,
	})
}

func runWallet(ctx context.Context, o *opts, args []string) error {
	sub := "show"
	if len(args) > 0 {
		sub = args[0]
		// Flags may follow the subcommand: 'wallet new --network calibration'.
		fs := flag.NewFlagSet("wallet "+sub, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		// Registering a flag resets it to its default, so keep what was
		// given before the subcommand.
		saved, store := *o, storeFlags
		commands["wallet"].flags(fs, o)
		*o, storeFlags = saved, store
		if err := fs.Parse(args[1:]); err != nil {
			return fmt.Errorf("ipfs2fil wallet %s: %v; run 'ipfs2fil help wallet'", sub, err)
		}
		args = append(args[:1], fs.Args()...)
	}
	switch {
	case sub == "show" && len(args) <= 1:
		return walletShow(ctx, o)
	case sub == "new" && len(args) == 1:
		return walletNew(o)
	default:
		return fmt.Errorf("no 'wallet %s'; use 'ipfs2fil wallet new' or 'ipfs2fil wallet'", strings.Join(args, " "))
	}
}

func walletNew(o *opts) error {
	path := storeFlags.keyFile
	if path == "" {
		path = os.Getenv(foc.EnvKeyFile)
	}
	byDefault := path == ""
	if byDefault {
		// LoadKey takes $IPFS2FIL_PRIVATE_KEY over the default file, which
		// would leave the new key unused.
		if os.Getenv(foc.EnvKey) != "" {
			return errors.New("$" + foc.EnvKey + " is set and would be used instead; unset it to make a new key")
		}
		p, err := foc.DefaultKeyFile()
		if err != nil {
			return err
		}
		path = p
	}
	key, err := foc.NewKeyFile(path)
	if err != nil {
		return err
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	testnet := storeFlags.network != "mainnet"
	fil, err := foc.FilecoinAddress(addr, testnet)
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	w := o.stdout
	fmt.Fprintf(w, "made a new wallet key in %s (readable only by you)\n\n", path)
	fmt.Fprintf(w, "address  %s\n         %s (the same account, for Filecoin wallets and exchanges)\n\n", addr.Hex(), fil)
	fmt.Fprintln(w, "Back that file up now: it is the only copy of the key.")
	if testnet {
		fmt.Fprintln(w, "Fund the address with test FIL and test USDFC from the calibration faucets.")
	} else {
		fmt.Fprintln(w, "Fund the address with a little FIL for gas and USDFC to pay for storage.")
		fmt.Fprintln(w, "If you send from an exchange, check it can withdraw to a 0x or f410 address.")
	}
	fmt.Fprintln(w, "Then 'ipfs2fil wallet' shows the balances.")
	if !byDefault {
		fmt.Fprintf(w, "Pass --key-file %s (or set $%s) to use it.\n", path, foc.EnvKeyFile)
	}
	return nil
}

// balancer is a store that knows the wallet's balances (FOC does).
type balancer interface {
	Balances(ctx context.Context) (foc.Balances, error)
}

var _ balancer = (*foc.Store)(nil)

type walletResult struct {
	Address   string `json:"address"`
	Filecoin  string `json:"filecoinAddress"`
	FIL       string `json:"fil"`
	USDFC     string `json:"usdfc"`
	Deposit   string `json:"deposit"`
	Available string `json:"available"`
}

func walletShow(ctx context.Context, o *opts) error {
	s, closer, err := openStore(ctx, o)
	if err != nil {
		return err
	}
	defer closer.Close()
	b, ok := s.(balancer)
	if !ok {
		return errors.New("this store has no wallet")
	}
	bal, err := b.Balances(ctx)
	if err != nil {
		return err
	}
	fil, err := foc.FilecoinAddress(bal.Address, bal.Testnet)
	if err != nil {
		return err
	}
	// FIL and USDFC both have 18 decimals.
	res := walletResult{Address: bal.Address.Hex(), Filecoin: fil, FIL: foc.USDFC(bal.FIL), USDFC: foc.USDFC(bal.USDFC),
		Deposit: foc.USDFC(bal.Deposit), Available: foc.USDFC(bal.Available)}
	if o.json {
		e := json.NewEncoder(o.stdout)
		e.SetIndent("", "  ")
		return e.Encode(res)
	}
	w := o.stdout
	fmt.Fprintf(w, "address  %s\n         %s\n", res.Address, res.Filecoin)
	fmt.Fprintf(w, "FIL      %s (for gas)\n", res.FIL)
	fmt.Fprintf(w, "USDFC    %s in the wallet\n", res.USDFC)
	fmt.Fprintf(w, "         %s deposited in Filecoin Pay (%s not locked up by data sets)\n", res.Deposit, res.Available)
	if bal.FIL.Sign() == 0 {
		fmt.Fprintln(w, "\nThe wallet has no FIL; send it some for gas before 'ipfs2fil push'.")
	}
	return nil
}
