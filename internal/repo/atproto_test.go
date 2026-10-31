package repo

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto/atprototest"
)

func TestATProto(t *testing.T) {
	a := atprototest.New(t)
	ATProtoDir = t.TempDir()
	accounts = sync.Map{}
	ctx := context.Background()

	if p, err := Discover("at://alice.test"); err != nil || p != "at://alice.test" {
		t.Fatalf("Discover = %q, %v", p, err)
	}
	r, err := Open("at://alice.test")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != KindATProto || Describe(r) != "Bluesky account @alice.test, rev 3zzzzzzzzzzzz" {
		t.Errorf("%s: %s", r.Kind(), Describe(r))
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 3 || !pins[0].Root.Equals(a.Commit) || pins[0].Mode != Recursive || pins[0].Name != "@alice.test" ||
		pins[1].Mode != Direct || pins[1].Name != "@alice.test did.json" || pins[2].Name != "@alice.test plc-audit-log.json" {
		t.Fatalf("pins %+v", pins)
	}
	got := map[string]int{}
	err = r.ForEachBlock(ctx, func(h multihash.Multihash, size int) error {
		data, err := r.Get(ctx, h)
		if err != nil || len(data) != size {
			t.Errorf("block %s: %d bytes, listed %d: %v", h.B58String(), len(data), size, err)
		}
		got[string(h)]++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 2 // the DID document and the PLC log
	for c := range a.Blocks {
		want++
		if got[string(c.Hash())] != 1 {
			t.Errorf("block %s listed %d times", c, got[string(c.Hash())])
		}
	}
	if len(got) != want {
		t.Errorf("%d blocks, want %d", len(got), want)
	}
	if b, err := r.Get(ctx, a.Image.Hash()); err != nil || !bytes.Equal(b, a.Blocks[a.Image]) {
		t.Errorf("image blob: %v", err)
	}
	if _, ok, _ := r.MFSRoot(ctx); ok {
		t.Error("an account has no MFS root")
	}

	// The same process opens the account once.
	before := a.Requests("com.atproto.sync.getRepo")
	r2, _ := Open("at://alice.test")
	r2.Pins(ctx)
	if a.Requests("com.atproto.sync.getRepo") != before {
		t.Error("fetched the repo again in the same run")
	}

	// With the server gone, a later run uses what it fetched, by handle or DID.
	a.Server.Close()
	for _, acct := range []string{"at://alice.test", a.DID} {
		accounts = sync.Map{}
		r, err := Open(acct)
		if err != nil {
			t.Fatal(err)
		}
		pins, err := r.Pins(ctx)
		if err != nil || len(pins) != 3 || !pins[0].Root.Equals(a.Commit) {
			t.Errorf("%s offline: %+v %v", acct, pins, err)
		}
	}
	accounts = sync.Map{}
	r, _ = Open("at://bob.test")
	if _, err := r.Pins(ctx); err == nil {
		t.Error("an account never fetched opened offline")
	}
}
