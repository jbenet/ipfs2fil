package store_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/memstore"
)

// carOf writes a CARv1 with these blocks. The header is not parsed by
// CheckCAR beyond being present, so a placeholder stands in for dag-cbor.
func carOf(blocks ...string) []byte {
	var b bytes.Buffer
	put := func(parts ...[]byte) {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		b.Write(binary.AppendUvarint(nil, uint64(n)))
		for _, p := range parts {
			b.Write(p)
		}
	}
	put([]byte("header"))
	for _, s := range blocks {
		h, _ := mh.Sum([]byte(s), mh.SHA2_256, -1)
		put(cid.NewCidV1(cid.Raw, h).Bytes(), []byte(s))
	}
	return b.Bytes()
}

// fixture writes n pieces of two blocks each and a manifest listing them.
func fixture(t *testing.T, n int) (*manifest.Manifest, []archive.Piece) {
	dir := t.TempDir()
	m := &manifest.Manifest{Root: "bafyroot2", Snapshots: []manifest.Snapshot{{Root: "bafyroot1"}, {Root: "bafyroot2"}}}
	var pieces []archive.Piece
	for i := range n {
		car := carOf(fmt.Sprint("a", i), fmt.Sprint("b", i))
		p := archive.Piece{Piece: manifest.Piece{Index: i, PieceCID: fmt.Sprint("bafkpiece", i), CARBytes: int64(len(car)), Blocks: 2},
			Path: filepath.Join(dir, fmt.Sprint(i, ".car"))}
		if err := os.WriteFile(p.Path, car, 0o644); err != nil {
			t.Fatal(err)
		}
		m.Pieces = append(m.Pieces, p.Piece)
		pieces = append(pieces, p)
	}
	return m, pieces
}

type recorder struct {
	mu     sync.Mutex
	events []store.PushEvent
	saves  int
}

func (r *recorder) progress(e store.PushEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}
func (r *recorder) save(*manifest.Manifest) error {
	r.mu.Lock()
	r.saves++
	r.mu.Unlock()
	return nil
}

func (r *recorder) count(f func(store.PushEvent) bool) int {
	n := 0
	for _, e := range r.events {
		if f(e) {
			n++
		}
	}
	return n
}

func final(e store.PushEvent) bool { return e.Final && e.Err == nil }

func TestPushSkipsOnRerun(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(5)
	m, pieces := fixture(t, 4)

	var r recorder
	if err := store.Push(ctx, s, m, pieces, 3, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 12 || len(m.Copies) != 3 {
		t.Fatalf("%d uploads, %d copies; want 12, 3", s.Uploads(), len(m.Copies))
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != 4 || c.Service != "mem" {
			t.Errorf("copy %+v", c)
		}
		if got := s.Meta(c.DataSetID, "bafkpiece0")["ipfsRootCID"]; got != "bafyroot2" {
			t.Errorf("ipfsRootCID %q, want the archive root", got)
		}
	}
	if n := r.count(func(e store.PushEvent) bool { return final(e) && !e.Skipped }); n != 12 {
		t.Errorf("%d final upload events, want 12", n)
	}

	// Re-run: same data sets, nothing uploaded, every piece reported skipped,
	// and no manifest save beyond the first.
	var again recorder
	if err := store.Push(ctx, s, m, pieces, 3, again.save, again.progress); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 12 || len(m.Copies) != 3 || again.saves != 1 {
		t.Fatalf("re-run: %d uploads, %d copies, %d saves", s.Uploads()-12, len(m.Copies), again.saves)
	}
	if n := again.count(func(e store.PushEvent) bool { return e.Skipped }); n != 12 {
		t.Errorf("%d skipped events, want 12", n)
	}
}

func TestPushResumesWithLostManifest(t *testing.T) {
	// The store has the pieces but the manifest lost its copies: Push
	// rebuilds them from the store without uploading again.
	ctx := context.Background()
	s := memstore.New(3)
	m, pieces := fixture(t, 2)
	if err := store.Push(ctx, s, m, pieces, 3, func(*manifest.Manifest) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	want := m.Copies
	m.Copies = nil
	if err := store.Push(ctx, s, m, pieces, 3, func(*manifest.Manifest) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 6 || fmt.Sprint(m.Copies) != fmt.Sprint(want) {
		t.Fatalf("got %v after %d uploads, want %v", m.Copies, s.Uploads(), want)
	}
}

func TestPushReportsMissingCAR(t *testing.T) {
	s := memstore.New(3)
	m, pieces := fixture(t, 2)
	os.Remove(pieces[1].Path)
	var r recorder
	err := store.Push(context.Background(), s, m, pieces, 2, r.save, r.progress)
	if err == nil || !strings.Contains(err.Error(), "piece 1") {
		t.Fatalf("err = %v", err)
	}
	if n := r.count(func(e store.PushEvent) bool { return e.Err != nil }); n != 2 {
		t.Errorf("%d error events, want one per copy", n)
	}
	if len(m.Copies[0].PieceIDs) != 1 {
		t.Errorf("piece 0 not recorded: %v", m.Copies)
	}
}

func TestPushNeedsSnapshot(t *testing.T) {
	err := store.Push(context.Background(), memstore.New(3), &manifest.Manifest{}, nil, 3, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "run pack") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	s := memstore.New(3)
	m, pieces := fixture(t, 3)
	if err := store.Push(ctx, s, m, pieces, 2, func(*manifest.Manifest) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var done int
	if err := store.Verify(ctx, s, m, func(e store.VerifyEvent) {
		mu.Lock()
		defer mu.Unlock()
		if e.Final && e.Err == nil {
			done++
		}
	}); err != nil || done != 6 {
		t.Fatalf("verify: %v, %d pieces checked, want 6", err, done)
	}

	s.Corrupt("bafkpiece1", int(m.Pieces[1].CARBytes)-1) // last byte of the last block
	err := store.Verify(ctx, s, m, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match its hash") || !strings.Contains(err.Error(), "piece 1") {
		t.Fatalf("corrupt piece: %v", err)
	}

	m.Pieces[0].Blocks = 3
	if err := store.Verify(ctx, s, m, nil); err == nil || !strings.Contains(err.Error(), "got 2 blocks") {
		t.Fatalf("block count: %v", err)
	}
}

func TestCheckCARTruncated(t *testing.T) {
	car := carOf("x", "y")
	if _, err := store.CheckCAR(bytes.NewReader(car[:len(car)-1])); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v", err)
	}
}

func TestPushReportsBytes(t *testing.T) {
	// A small piece still reports its bytes before the final event.
	s := memstore.New(1)
	m, pieces := fixture(t, 1)
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 1, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if len(r.events) != 2 || r.events[0].Final || r.events[0].Done != pieces[0].CARBytes || !r.events[1].Final {
		t.Fatalf("events %+v", r.events)
	}
}
