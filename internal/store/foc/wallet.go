package foc

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/filecoin-project/go-address"
)

// Where the wallet key comes from, in order.
const (
	EnvKey     = "IPFS2FIL_PRIVATE_KEY" // hex secp256k1 key, with or without 0x
	EnvKeyFile = "IPFS2FIL_KEY_FILE"    // a file holding the same, or a Lotus key export
	EnvAddress = "IPFS2FIL_ADDRESS"     // 0x wallet address, for read-only use without a key
	EnvHome    = "IPFS2FIL_HOME"        // holds wallet.key, the key 'ipfs2fil wallet new' makes
)

// DefaultKeyFile is where 'ipfs2fil wallet new' writes a key and where
// LoadKey looks last: wallet.key in $IPFS2FIL_HOME, else ~/.ipfs2fil.
func DefaultKeyFile() (string, error) {
	if d := os.Getenv(EnvHome); d != "" {
		return filepath.Join(d, "wallet.key"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding your home directory: %w; set $%s", err, EnvHome)
	}
	return filepath.Join(home, ".ipfs2fil", "wallet.key"), nil
}

// NewKeyFile makes a new wallet key and writes it, hex, to path, readable
// only by you. It never overwrites a file: losing a funded key loses its
// funds and the data sets it pays for.
func NewKeyFile(path string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%s already exists; not replacing a wallet key (move it away first, or use --key-file for another path)", path)
	}
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintln(f, hex.EncodeToString(crypto.FromECDSA(key)))
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, nil
}

// FilecoinAddress is the f410 (or t410 on calibration) address of a 0x
// address: the same account, in the form Filecoin wallets and exchanges use.
func FilecoinAddress(addr common.Address, testnet bool) (string, error) {
	a, err := address.NewDelegatedAddress(10, addr.Bytes())
	if err != nil {
		return "", err
	}
	s := a.String()
	if testnet {
		s = "t" + s[1:]
	}
	return s, nil
}

// ReadConfig is the Config for commands that only read (status, verify):
// the wallet key if one is set, else address, else $IPFS2FIL_ADDRESS.
func ReadConfig(rpc, keyFile, address string) (Config, error) {
	key, err := LoadKey(keyFile)
	if err == nil {
		return Config{RPC: rpc, Key: key}, nil
	}
	if err != errNoKey {
		return Config{}, err
	}
	if address == "" {
		address = os.Getenv(EnvAddress)
	}
	if address == "" {
		return Config{}, errors.New("no wallet: set --address or " + EnvAddress + " to the 0x address that paid for the archive, or give a wallet key")
	}
	return Config{RPC: rpc, Address: address}, nil
}

var errNoKey = errors.New("no wallet key: run 'ipfs2fil wallet new' to make one, or set " + EnvKey + " to a hex private key, or " +
	EnvKeyFile + " (or --key-file) to a file holding one; the wallet pays for storage")

// LoadKey reads the wallet key from keyFile, else $IPFS2FIL_KEY_FILE, else
// $IPFS2FIL_PRIVATE_KEY, else DefaultKeyFile if it exists. Errors never
// include key material.
func LoadKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if keyFile == "" {
		keyFile = os.Getenv(EnvKeyFile)
	}
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("reading the wallet key file: %w", err)
		}
		k, err := parseKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("wallet key file %s: %w", keyFile, err)
		}
		return k, nil
	}
	if v := os.Getenv(EnvKey); v != "" {
		k, err := parseKey(v)
		if err != nil {
			return nil, fmt.Errorf("$%s: %w", EnvKey, err)
		}
		return k, nil
	}
	if p, err := DefaultKeyFile(); err == nil {
		if _, err := os.Stat(p); err == nil {
			return LoadKey(p)
		}
	}
	return nil, errNoKey
}

// parseKey accepts a hex secp256k1 key or a Lotus export (hex of JSON
// {"Type":"secp256k1","PrivateKey":"<base64>"}).
func parseKey(s string) (*ecdsa.PrivateKey, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "0x")
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, errors.New("not a hex private key")
	}
	if len(raw) > 32 && raw[0] == '{' {
		var lk struct{ Type, PrivateKey string }
		if json.Unmarshal(raw, &lk) != nil {
			return nil, errors.New("not a hex private key or a Lotus key export")
		}
		if lk.Type != "secp256k1" {
			return nil, errors.New("BLS keys are not supported; use a secp256k1 (f1/0x) key")
		}
		if raw, err = base64.StdEncoding.DecodeString(lk.PrivateKey); err != nil {
			return nil, errors.New("the Lotus key export has a bad PrivateKey")
		}
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("a private key is 32 bytes, this is %d", len(raw))
	}
	k, err := crypto.ToECDSA(raw)
	if err != nil {
		return nil, errors.New("not a valid secp256k1 private key")
	}
	return k, nil
}
