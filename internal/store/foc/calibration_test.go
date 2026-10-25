//go:build calibration

// This test runs against Filecoin Calibration and spends test tokens. Run it
// with scripts/calibration.sh from a machine that can reach glif.
package foc_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/strahe/synapse-go/piece"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
	"github.com/jbenet/ipfs2fil/internal/store"
	"github.com/jbenet/ipfs2fil/internal/store/foc"
)

func open(t *testing.T, ctx context.Context) *foc.Store {
	key, err := foc.LoadKey("")
	if err != nil {
		t.Skip(err)
	}
	rpc := os.Getenv("IPFS2FIL_RPC")
	if rpc == "" {
		rpc = foc.CalibrationRPC
	}
	s, closer, err := foc.Open(ctx, foc.Config{RPC: rpc, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closer.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// pieces writes n CARv1 files of about 1 MiB each, the same bytes every run,
// so a second run of this test finds them stored already.
func pieces(t *testing.T, n int) (*manifest.Manifest, []archive.Piece) {
	dir := t.TempDir()
	rng := rand.New(rand.NewPCG(1, 2))
	var ps []archive.Piece
	var root cid.Cid
	m := &manifest.Manifest{}
	for i := range n {
		var blocks [][]byte
		for range 16 {
			b := make([]byte, 64<<10)
			for j := range b {
				b[j] = byte(rng.Uint32())
			}
			blocks = append(blocks, b)
		}
		car, first := writeCAR(blocks)
		if i == 0 {
			root = first
		}
		info, err := piece.CalculateFromBytes(car)
		if err != nil {
			t.Fatal(err)
		}
		p := archive.Piece{
			Piece: manifest.Piece{Index: i, PieceCID: info.CIDv2.String(), CARBytes: int64(len(car)), Blocks: int64(len(blocks))},
			Path:  filepath.Join(dir, info.CIDv2.String()+".car"),
		}
		if err := os.WriteFile(p.Path, car, 0o644); err != nil {
			t.Fatal(err)
		}
		ps = append(ps, p)
		m.Pieces = append(m.Pieces, p.Piece)
	}
	m.Root = root.String()
	key := os.Getenv("IPFS2FIL_CALIBRATION_ARCHIVE")
	if key == "" {
		key = "ipfs2fil-calibration-" + root.String()[len(root.String())-8:]
	}
	m.Snapshots = []manifest.Snapshot{{Time: time.Now(), Root: key}}
	return m, ps
}

// writeCAR returns a CARv1 whose header names its first block.
func writeCAR(blocks [][]byte) ([]byte, cid.Cid) {
	var out bytes.Buffer
	section := func(parts ...[]byte) {
		n := 0
		for _, p := range parts {
			n += len(p)
		}
		out.Write(binary.AppendUvarint(nil, uint64(n)))
		for _, p := range parts {
			out.Write(p)
		}
	}
	cids := make([]cid.Cid, len(blocks))
	for i, b := range blocks {
		h, _ := mh.Sum(b, mh.SHA2_256, -1)
		cids[i] = cid.NewCidV1(cid.Raw, h)
	}
	// dag-cbor {"roots": [CID], "version": 1}
	cb := append([]byte{0x00}, cids[0].Bytes()...)
	hdr := []byte{0xa2, 0x65, 'r', 'o', 'o', 't', 's', 0x81, 0xd8, 0x2a, 0x58, byte(len(cb))}
	hdr = append(hdr, cb...)
	hdr = append(hdr, 0x67, 'v', 'e', 'r', 's', 'i', 'o', 'n', 0x01)
	section(hdr)
	for i, b := range blocks {
		section(cids[i].Bytes(), b)
	}
	return out.Bytes(), cids[0]
}

func TestCalibration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	m, ps := pieces(t, 3)
	key, _ := store.ArchiveKey(m)
	t.Logf("archive key %s, root %s", key, m.Root)

	s := open(t, ctx)
	var sizes []int64
	for _, p := range ps {
		sizes = append(sizes, p.CARBytes)
	}
	f, err := s.Plan(ctx, key, 3, sizes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: deposit %s USDFC, approve %v, monthly %s, fees %s, wallet %s",
		foc.USDFC(f.Deposit), f.Approve, foc.USDFC(f.Monthly), foc.USDFC(f.Fees), foc.USDFC(f.Wallet))
	if !f.Ready() {
		if os.Getenv("IPFS2FIL_CALIBRATION_FUND") != "1" {
			t.Fatal("funding needed; rerun with IPFS2FIL_CALIBRATION_FUND=1 to send it")
		}
		if err := f.Apply(ctx); err != nil {
			t.Fatal(err)
		}
	}

	save := func(*manifest.Manifest) error { return nil }
	log := func(e store.PushEvent) {
		if e.Done == e.Total || e.Err != nil {
			t.Logf("copy %d provider %s piece %d: skipped=%v err=%v", e.Copy, e.Provider, e.Piece, e.Skipped, e.Err)
		}
	}
	start := time.Now()
	if err := store.Push(ctx, s, m, ps, 3, save, log); err != nil {
		t.Fatal(err)
	}
	t.Logf("first push took %s; copies %+v", time.Since(start), m.Copies)

	// A fresh connection reads the pieces from chain and uploads nothing.
	s2 := open(t, ctx)
	var skipped atomic.Int32
	if err := store.Push(ctx, s2, m, ps, 3, save, func(e store.PushEvent) {
		if e.Skipped {
			skipped.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if n := skipped.Load(); n != 9 {
		t.Errorf("re-run skipped %d of 9 piece copies", n)
	}

	st, err := s2.Status(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.DataSets {
		t.Logf("data set %s on provider %s: %d pieces, last proven %s", d.ID, d.Provider, d.Pieces, d.LastProven)
	}
	t.Logf("funded days: %d", st.FundedDays)
	if len(st.DataSets) != 3 {
		t.Errorf("status shows %d data sets, want 3", len(st.DataSets))
	}

	if err := store.Verify(ctx, s2, m, nil); err != nil {
		t.Fatal(err)
	}
	fmt.Println("calibration: stored 3 copies, re-run uploaded nothing, verify passed")
}
