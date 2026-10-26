package collection

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestRecord(t *testing.T) {
	secret := Secret([]byte("wallet key bytes"))
	r := Record{Host: "laptop", Name: "photos", Added: time.Date(2026, 10, 26, 5, 0, 0, 123, time.UTC)}
	a, err := SealRecord(secret, r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := SealRecord(secret, r)
	if !bytes.Equal(a, b) {
		t.Error("the same record sealed to different bytes")
	}
	if bytes.Contains(a, []byte("photos")) || bytes.Contains(a, []byte("laptop")) {
		t.Error("the sealed record shows its names")
	}
	got, err := OpenRecord(secret, a)
	if err != nil || got != r {
		t.Fatalf("opened %+v, %v", got, err)
	}
	if _, err := OpenRecord(Secret([]byte("another wallet")), a); !errors.Is(err, ErrNotRecord) {
		t.Errorf("another wallet opened it: %v", err)
	}
	a[len(a)-1] ^= 1
	if _, err := OpenRecord(secret, a); !errors.Is(err, ErrNotRecord) {
		t.Errorf("a changed record opened: %v", err)
	}
}
