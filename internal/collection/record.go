package collection

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Record names an archive to the other machines that share its data sets.
// It is sealed into the archive's own pieces (archive.Options.Record), so a
// machine reading the catalog from chain learns which repo a snapshot
// belongs to. It is encrypted under a key derived from the wallet key, since
// anyone can read the pieces.
type Record struct {
	Host  string    `json:"host"`
	Name  string    `json:"name"`
	Added time.Time `json:"added"`
}

// recordMagic starts every sealed record, so other blocks are told apart.
const recordMagic = "ipfs2fil-catalog/1\n"

// Secret derives the catalog key from the wallet's private key bytes. Every
// machine with the same wallet derives the same one.
func Secret(walletKey []byte) []byte {
	m := hmac.New(sha256.New, []byte("ipfs2fil catalog key v1"))
	m.Write(walletKey)
	return m.Sum(nil)
}

func subkey(secret []byte, use string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(use))
	return m.Sum(nil)
}

// SealRecord encrypts r with AES-GCM. The nonce comes from the record itself,
// so the same record seals to the same bytes and an unchanged repo packs to
// the same pieces.
func SealRecord(secret []byte, r Record) ([]byte, error) {
	r.Added = r.Added.UTC()
	plain, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	aead, err := recordAEAD(secret)
	if err != nil {
		return nil, err
	}
	nonce := subkey(subkey(secret, "nonce"), string(plain))[:aead.NonceSize()]
	out := append([]byte(recordMagic), nonce...)
	return aead.Seal(out, nonce, plain, []byte(recordMagic)), nil
}

// ErrNotRecord means a block is not a sealed record, or not one this wallet
// sealed.
var ErrNotRecord = errors.New("not a catalog record of this wallet")

// OpenRecord decrypts a block SealRecord made.
func OpenRecord(secret, sealed []byte) (Record, error) {
	var r Record
	aead, err := recordAEAD(secret)
	if err != nil {
		return r, err
	}
	if !bytes.HasPrefix(sealed, []byte(recordMagic)) || len(sealed) < len(recordMagic)+aead.NonceSize() {
		return r, ErrNotRecord
	}
	rest := sealed[len(recordMagic):]
	plain, err := aead.Open(nil, rest[:aead.NonceSize()], rest[aead.NonceSize():], []byte(recordMagic))
	if err != nil {
		return r, ErrNotRecord
	}
	if err := json.Unmarshal(plain, &r); err != nil {
		return r, fmt.Errorf("catalog record: %w", err)
	}
	if !nameOK.MatchString(r.Name) || !hostOK.MatchString(r.Host) {
		return r, fmt.Errorf("catalog record names %q on %q, which are not usable names", r.Name, r.Host)
	}
	return r, nil
}

func recordAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) == 0 {
		return nil, errors.New("no catalog key; it comes from the wallet key")
	}
	b, err := aes.NewCipher(subkey(secret, "encrypt"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}
