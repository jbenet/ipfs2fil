package repo

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/atproto/atprototest"
)

func TestPDS(t *testing.T) {
	a := atprototest.New(t)
	b := a.Host(t, "did:plc:testbobbbbbbbbbbbbbbbbbbb", "bob.test", "Bob")
	gone := a.Host(t, "did:plc:testgoneeeeeeeeeeeeeeeeee", "gone.test", "Gone")
	gone.Inactive = true
	ATProtoDir = t.TempDir()
	accounts = sync.Map{}
	pdsProbed = sync.Map{}
	ctx := context.Background()

	if !IsPDS(a.Server.URL) || !IsPDS("pds:"+a.Server.URL) {
		t.Fatal("the fake PDS is not detected")
	}
	if IsPDS(a.Server.URL+"/some/path") || IsPDS("pds:https://u:p@host") || IsPDS("/tmp/x") {
		t.Fatal("not a PDS, but detected")
	}
	if p, err := Discover(a.Server.URL); err != nil || p != a.Server.URL {
		t.Fatalf("Discover = %q, %v", p, err)
	}
	r, err := Open(a.Server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind() != KindPDS {
		t.Fatalf("kind %s", r.Kind())
	}
	if d := Describe(r); d == "" || d[len(d)-10:] != "2 accounts" {
		t.Errorf("Describe = %q", d)
	}
	pins, err := r.Pins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	var doc PDSDoc
	for _, p := range pins {
		names[p.Name] = true
		if p.Name == PDSFile {
			data, err := r.Get(ctx, p.Root.Hash())
			if err != nil || json.Unmarshal(data, &doc) != nil {
				t.Fatalf("pds.json: %v", err)
			}
		}
	}
	for _, n := range []string{"@alice.test", "@bob.test", "@bob.test did.json", PDSFile} {
		if !names[n] {
			t.Errorf("no pin %q in %v", n, names)
		}
	}
	if names["@gone.test"] {
		t.Error("an inactive account was archived")
	}
	if doc.DID != "did:web:pds.test" || len(doc.Accounts) != 3 {
		t.Fatalf("pds.json %+v", doc)
	}
	for _, ac := range doc.Accounts {
		switch ac.DID {
		case gone.DID:
			if ac.Skipped != "the server says it is deactivated" {
				t.Errorf("gone: %+v", ac)
			}
		case b.DID:
			if ac.Handle != "bob.test" || ac.Commit != b.Commit.String() || ac.Skipped != "" {
				t.Errorf("bob: %+v", ac)
			}
		}
	}

	// Every block of both accounts, each once, readable.
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
	for _, acct := range []*atprototest.Account{a, b} {
		for c := range acct.Blocks {
			if got[string(c.Hash())] != 1 {
				t.Errorf("block %s listed %d times", c, got[string(c.Hash())])
			}
		}
	}
	for c := range gone.Blocks {
		if _, ok := got[string(c.Hash())]; ok && gone.Blocks[c] != nil {
			if _, shared := a.Blocks[c]; !shared {
				t.Errorf("block %s of the inactive account archived", c)
			}
		}
	}
	// The image blob is the same bytes in every account: listed once.
	if got[string(a.Image.Hash())] != 1 {
		t.Errorf("shared blob listed %d times", got[string(a.Image.Hash())])
	}
}
