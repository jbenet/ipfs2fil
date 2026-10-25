package store

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ipfs/go-cid"

	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// VerifyEvent reports progress fetching one piece from one copy. The Final
// event says the piece checked out, or Err says why not.
type VerifyEvent struct {
	Copy        int // index into m.Copies
	Provider    string
	Piece       int
	Done, Total int64
	Final       bool
	Err         error
}

// Verify fetches every piece back from every copy in m.Copies and checks it:
// the bytes must hash to the PieceCID (the store's reader checks that at EOF),
// and every block in the CAR must hash to its CID, with the block count the
// manifest records. Copies are checked in parallel. Missing or bad pieces are
// reported through progress and returned joined; verify keeps going past them.
func Verify(ctx context.Context, s Store, m *manifest.Manifest, progress func(VerifyEvent)) error {
	if len(m.Copies) == 0 {
		return errors.New("the manifest records no copies; run push first")
	}
	if progress == nil {
		progress = func(VerifyEvent) {}
	}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for i, c := range m.Copies {
		ds := DataSet{Provider: c.Provider, ID: c.DataSetID}
		wg.Go(func() {
			for _, p := range m.Pieces {
				ev := VerifyEvent{Copy: i, Provider: c.Provider, Piece: p.Index, Total: p.CARBytes}
				err := verifyOne(ctx, s, ds, p, func(done int64) {
					ev.Done = done
					progress(ev)
				})
				if err != nil {
					ev.Err, ev.Final = err, true
					progress(ev)
					mu.Lock()
					errs = append(errs, fmt.Errorf("copy %d on provider %s, piece %d: %w", i+1, c.Provider, p.Index, err))
					mu.Unlock()
					if ctx.Err() != nil {
						return
					}
					continue
				}
				ev.Done, ev.Final = p.CARBytes, true
				progress(ev)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func verifyOne(ctx context.Context, s Store, ds DataSet, p manifest.Piece, sent func(int64)) error {
	rc, err := s.Fetch(ctx, ds, p.PieceCID)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := &counter{r: rc, report: sent}
	blocks, err := CheckCAR(cr)
	if err != nil {
		return err
	}
	// Drain to EOF so the store's PieceCID check runs.
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return fmt.Errorf("PieceCID check: %w", err)
	}
	if cr.n != p.CARBytes {
		return fmt.Errorf("got %d bytes, the manifest says %d", cr.n, p.CARBytes)
	}
	if blocks != p.Blocks {
		return fmt.Errorf("got %d blocks, the manifest says %d", blocks, p.Blocks)
	}
	return nil
}

// CheckCAR reads a CARv1 stream and hash-checks every block. It returns the
// number of blocks.
func CheckCAR(r io.Reader) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<20)
	hdr, err := section(br)
	if err != nil {
		return 0, fmt.Errorf("CAR header: %w", err)
	}
	if len(hdr) == 0 {
		return 0, errors.New("CAR header: empty")
	}
	var n int64
	for {
		b, err := section(br)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("block %d: %w", n, err)
		}
		k, c, err := cid.CidFromBytes(b)
		if err != nil {
			return n, fmt.Errorf("block %d: bad CID: %w", n, err)
		}
		got, err := c.Prefix().Sum(b[k:])
		if err != nil {
			return n, fmt.Errorf("block %s: %w", c, err)
		}
		if !got.Equals(c) {
			return n, fmt.Errorf("block %s does not match its hash", c)
		}
		n++
	}
}

const maxSection = 8 << 20 // larger than any block kubo writes

// section reads one varint-length-prefixed CAR section.
func section(br *bufio.Reader) ([]byte, error) {
	l, err := binary.ReadUvarint(br)
	if err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}
	if l > maxSection {
		return nil, fmt.Errorf("section of %d bytes is too large", l)
	}
	b := make([]byte, l)
	if _, err := io.ReadFull(br, b); err != nil {
		return nil, fmt.Errorf("truncated: %w", err)
	}
	return b, nil
}
