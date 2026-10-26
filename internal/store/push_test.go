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
	"time"

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
	// A small piece still reports its bytes, then that it is staged, before
	// the final event.
	s := memstore.New(1)
	m, pieces := fixture(t, 1)
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 1, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if len(r.events) != 3 || r.events[0].Final || r.events[0].Done != pieces[0].CARBytes ||
		!r.events[1].Staged || r.events[1].Final || !r.events[2].Final {
		t.Fatalf("events %+v", r.events)
	}
}

func TestPushPullsFromFirstCopy(t *testing.T) {
	// Each piece leaves this machine once; the other copies' providers
	// fetch it from the first copy's provider.
	s := memstore.New(5)
	m, pieces := fixture(t, 4)
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 3, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 12 || s.Pulls() != 8 {
		t.Fatalf("%d stored, %d pulled; want 12, 8", s.Uploads(), s.Pulls())
	}
	first := m.Copies[0].Provider
	if n := r.count(func(e store.PushEvent) bool { return final(e) && e.Pulled && e.From == first && e.Copy > 0 }); n != 8 {
		t.Errorf("%d pulled events from provider %s, want 8", n, first)
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != 4 {
			t.Errorf("copy %+v is missing pieces", c)
		}
	}

	// A copy added later pulls from the first copy too.
	if err := store.Push(context.Background(), s, m, pieces, 4, r.save, nil); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 16 || s.Pulls() != 12 {
		t.Fatalf("fourth copy: %d stored, %d pulled; want 16, 12", s.Uploads(), s.Pulls())
	}
}

func TestPushUploadsWhenPullFails(t *testing.T) {
	s := memstore.New(3)
	s.PullFails = true
	m, pieces := fixture(t, 2)
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 3, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 6 || s.Pulls() != 0 {
		t.Fatalf("%d stored, %d pulled; want 6, 0", s.Uploads(), s.Pulls())
	}
	if n := r.count(func(e store.PushEvent) bool { return e.PullErr != nil && !e.Final }); n != 4 {
		t.Errorf("%d pull failures reported, want 4", n)
	}
}

// gate holds every Commit that adds pieces until open is closed, as a wait
// for the base fee to fall.
type gate struct {
	open    chan struct{}
	waiting chan struct{} // receives once per held Commit
}

func newGate() *gate { return &gate{open: make(chan struct{}), waiting: make(chan struct{}, 100)} }

func (g *gate) hold(ctx context.Context, _ store.DataSet, _ []string) error {
	g.waiting <- struct{}{}
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestPushSendsWhileAddsWait(t *testing.T) {
	// While adding on chain waits, every piece still reaches every provider;
	// once it may go ahead, what piled up is added in one step per copy.
	s := memstore.New(3)
	g := newGate()
	s.BeforeCommit = g.hold
	m, pieces := fixture(t, 4)
	var r recorder
	done := make(chan error)
	go func() { done <- store.Push(context.Background(), s, m, pieces, 3, r.save, r.progress) }()
	deadline := time.Now().Add(10 * time.Second)
	for s.Uploads() < 12 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 12 pieces staged while adds wait", s.Uploads())
		}
		time.Sleep(time.Millisecond)
	}
	close(g.open)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Each copy's first add started before the other pieces were sent;
	// the rest go together.
	if n := s.Commits(); n < 3 || n > 6 {
		t.Errorf("%d on-chain adds, want 3 to 6 for 12 pieces", n)
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != 4 {
			t.Errorf("copy %+v is missing pieces", c)
		}
	}
	if n := r.count(func(e store.PushEvent) bool { return e.Staged }); n != 12 {
		t.Errorf("%d staged events, want 12", n)
	}
}

func TestPushStopsSendingFarAhead(t *testing.T) {
	// Providers drop staged pieces after a while, so a copy stops sending
	// once its oldest waiting piece is stageAhead old.
	defer store.SetStageAhead(0)()
	s := memstore.New(1)
	g := newGate()
	s.BeforeCommit = g.hold
	m, pieces := fixture(t, 3)
	done := make(chan error)
	go func() {
		done <- store.Push(context.Background(), s, m, pieces, 1, func(*manifest.Manifest) error { return nil }, nil)
	}()
	<-g.waiting
	time.Sleep(50 * time.Millisecond)
	if n := s.Uploads(); n != 1 {
		t.Fatalf("%d pieces sent while the first waits to be added, want 1", n)
	}
	close(g.open)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 3 || len(m.Copies[0].PieceIDs) != 3 {
		t.Fatalf("%d sent, %d stored; want 3, 3", s.Uploads(), len(m.Copies[0].PieceIDs))
	}
}

func TestPushResendsDroppedPieces(t *testing.T) {
	// A provider dropped a staged piece before it was added (FOC providers
	// do after about two hours): it is sent again and added.
	s := memstore.New(2)
	m, pieces := fixture(t, 2)
	dropped := false
	s.BeforeCommit = func(_ context.Context, ds store.DataSet, cids []string) error {
		if ds.Provider == "2" && !dropped {
			dropped = true
			s.Drop(ds.Provider, cids[0])
		}
		return nil
	}
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 2, r.save, r.progress); err != nil {
		t.Fatal(err)
	}
	if n := r.count(func(e store.PushEvent) bool { return e.Dropped }); n != 1 {
		t.Errorf("%d dropped events, want 1", n)
	}
	// 2 pieces x 2 copies, the second copy pulled, plus one sent again.
	if s.Uploads() != 5 {
		t.Errorf("%d pieces sent, want 5", s.Uploads())
	}
	for _, c := range m.Copies {
		if len(c.PieceIDs) != 2 {
			t.Errorf("copy %+v is missing pieces", c)
		}
	}
}

func TestPushResumeSendsNothingTwice(t *testing.T) {
	// A push stopped while pieces wait to be added: the re-run skips the
	// piece added before and adds the staged ones without sending them again.
	s := memstore.New(1)
	m, pieces := fixture(t, 4)
	save := func(*manifest.Manifest) error { return nil }
	if err := store.Push(context.Background(), s, m, pieces[:1], 1, save, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.BeforeCommit = func(ctx context.Context, _ store.DataSet, _ []string) error {
		for s.Uploads() < 4 {
			time.Sleep(time.Millisecond)
		}
		cancel()
		return ctx.Err()
	}
	if err := store.Push(ctx, s, m, pieces, 1, save, nil); err == nil {
		t.Fatal("the stopped push reported no error")
	}
	if n := len(m.Copies[0].PieceIDs); n != 1 || s.Uploads() != 4 {
		t.Fatalf("stopped push: %d added, %d sent; want 1, 4", n, s.Uploads())
	}

	s.BeforeCommit = nil
	var r recorder
	if err := store.Push(context.Background(), s, m, pieces, 1, save, r.progress); err != nil {
		t.Fatal(err)
	}
	if s.Uploads() != 4 || len(m.Copies[0].PieceIDs) != 4 {
		t.Fatalf("re-run: %d sent in all, %d added; want 4, 4", s.Uploads(), len(m.Copies[0].PieceIDs))
	}
	if n := r.count(func(e store.PushEvent) bool { return e.Skipped }); n != 1 {
		t.Errorf("%d skipped, want 1", n)
	}
	if n := r.count(func(e store.PushEvent) bool { return e.Done > 0 && !e.Staged && !e.Final }); n != 0 {
		t.Errorf("re-run sent bytes %d times", n)
	}
}
