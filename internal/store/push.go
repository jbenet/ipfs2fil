package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/jbenet/ipfs2fil/internal/archive"
	"github.com/jbenet/ipfs2fil/internal/manifest"
)

// ArchiveKey is the key Store methods take: the root of the first snapshot,
// or for an archive in a collection, the collection's key, so every archive
// in it shares one set of data sets.
func ArchiveKey(m *manifest.Manifest) (string, error) {
	if m.Collection != "" {
		return m.Collection, nil
	}
	if len(m.Snapshots) == 0 || m.Snapshots[0].Root == "" {
		return "", errors.New("the manifest has no snapshots; run pack first")
	}
	return m.Snapshots[0].Root, nil
}

// PushEvent reports progress on one piece in one copy. Events for different
// copies arrive from different goroutines.
type PushEvent struct {
	Copy     int    // index into the data sets, 0..copies-1
	Provider string // the copy's provider
	Piece    int    // manifest piece index
	// Done and Total are CAR bytes sent so far and in all. Done reaches
	// Total when the bytes are sent, before the piece is added on chain.
	Done, Total int64
	// Staged marks the non-final event sent once the provider holds the
	// piece, ready to add. Adding it on chain may wait, for a high base fee
	// or for pieces ahead of it, while later pieces are sent.
	Staged bool
	// Final marks the last event for this piece in this copy: it is stored
	// (or Skipped), or Err says why not.
	Final   bool
	Skipped bool // the copy already held this piece
	// Pulled is set when the copy's provider fetched the piece from the
	// first copy's provider (From) instead of from this machine.
	Pulled bool
	From   string
	// PullErr says why a provider could not fetch the piece from the first
	// copy, on a non-final event; the piece is then uploaded from here.
	PullErr error
	// Dropped marks a non-final event: the provider dropped the staged piece
	// before it could be added, so it is sent again.
	Dropped bool
	Err     error // this copy stopped at this piece
}

// stageAhead bounds how far sending runs ahead of adding on chain: a copy
// stops sending while the oldest piece it has waiting to be added was staged
// this long ago. FOC providers drop staged pieces about two hours after they
// arrive, so sending further ahead during a long wait for the base fee would
// only send the same pieces twice.
var stageAhead = time.Hour

// commitBatch is the most pieces added in one on-chain step.
const commitBatch = 32

// Push stores every piece in copies data sets. Pieces a data set already
// holds are skipped, so a re-run after a crash or an edit uploads only what is
// missing. Copies upload in parallel. m.Copies is updated as pieces land, and
// save is called after each one so a killed run loses at most the pieces in
// flight. Each piece carries ipfsRootCID = m.Root.
//
// Sending a piece and adding it on chain are separate steps. Each copy keeps
// sending pieces to its provider while earlier ones wait to be added, and adds
// whatever has piled up in one step, so a wait for the base fee to fall does
// not stop the uploads. A piece the provider dropped while it waited is sent
// again.
//
// The first copy uploads each piece and the others have their providers pull
// it from the first copy's provider once it is there, falling back to an
// upload if the pull fails or the first copy does not get the piece.
//
// A failing copy stops at its first error; the other copies carry on, and
// Push returns all errors joined.
func Push(ctx context.Context, s Store, m *manifest.Manifest, pieces []archive.Piece, copies int,
	save func(*manifest.Manifest) error, progress func(PushEvent)) error {
	key, err := ArchiveKey(m)
	if err != nil {
		return err
	}
	if progress == nil {
		progress = func(PushEvent) {}
	}
	sets, err := s.DataSets(ctx, key, copies)
	if err != nil {
		return err
	}
	service := "unknown"
	if n, ok := s.(interface{ Service() string }); ok {
		service = n.Service()
	}

	var mu sync.Mutex                 // guards m and save
	records := make([]int, len(sets)) // index into m.Copies
	mu.Lock()
	for i, ds := range sets {
		records[i] = copyRecord(m, ds, service)
	}
	err = save(m)
	mu.Unlock()
	if err != nil {
		return fmt.Errorf("saving the manifest: %w", err)
	}

	// landed[j] closes when the first copy is done with pieces[j]; held[j]
	// then says whether its provider holds it, so the others can pull it.
	var landed []chan struct{}
	var held []bool
	if len(sets) > 1 {
		landed, held = make([]chan struct{}, len(pieces)), make([]bool, len(pieces))
		for j := range landed {
			landed[j] = make(chan struct{})
		}
	}
	errs := make([]error, len(sets))
	var wg sync.WaitGroup
	for i, ds := range sets {
		c := &copyPush{s: s, i: i, ds: ds, first: sets[0], pieces: pieces, progress: progress,
			meta: map[string]string{"ipfsRootCID": m.Root}, landed: landed, held: held,
			record: func(p archive.Piece, id string) error {
				mu.Lock()
				defer mu.Unlock()
				ids := m.Copies[records[i]].PieceIDs
				if ids[p.Index] == id {
					return nil
				}
				ids[p.Index] = id
				if err := save(m); err != nil {
					return fmt.Errorf("saving the manifest: %w", err)
				}
				return nil
			}}
		wg.Go(func() {
			if err := c.run(ctx); err != nil {
				errs[i] = fmt.Errorf("copy %d on provider %s: %w", i+1, ds.Provider, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// copyRecord finds or adds m's Copy for ds and returns its index.
func copyRecord(m *manifest.Manifest, ds DataSet, service string) int {
	for i := range m.Copies {
		if c := &m.Copies[i]; c.DataSetID == ds.ID && c.Provider == ds.Provider {
			if c.PieceIDs == nil {
				c.PieceIDs = map[int]string{}
			}
			if c.Service == "" {
				c.Service = service
			}
			return i
		}
	}
	m.Copies = append(m.Copies, manifest.Copy{Provider: ds.Provider, DataSetID: ds.ID, PieceIDs: map[int]string{}, Service: service})
	return len(m.Copies) - 1
}

// copyPush stores the pieces in one copy: a sender stages them with the
// provider while a committer adds the staged ones on chain in batches.
type copyPush struct {
	s         Store
	i         int
	ds, first DataSet
	pieces    []archive.Piece
	meta      map[string]string
	landed    []chan struct{}
	held      []bool
	progress  func(PushEvent)
	record    func(p archive.Piece, id string) error

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []staged // staged and waiting to be added, oldest first
	sending bool
	failed  bool // the committer stopped
}

type staged struct {
	p  archive.Piece
	at time.Time
	ev PushEvent
}

func (c *copyPush) run(ctx context.Context) error {
	c.cond = sync.NewCond(&c.mu)
	c.sending = true
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cond.Broadcast()
	})
	defer stop()
	committed := make(chan error, 1)
	go func() { committed <- c.commitLoop(ctx) }()
	sendErr := c.send(ctx)
	c.mu.Lock()
	c.sending = false
	c.cond.Broadcast()
	c.mu.Unlock()
	// What was staged before a send failed is still added.
	commitErr := <-committed
	if commitErr != nil {
		return commitErr
	}
	return sendErr
}

// send stages each piece the copy lacks and queues it to be added.
func (c *copyPush) send(ctx context.Context) error {
	next := 0
	if c.landed != nil && c.i == 0 {
		defer func() {
			for _, l := range c.landed[next:] {
				close(l)
			}
		}()
	}
	landed := func(j int, ok bool) {
		if c.landed != nil && c.i == 0 {
			c.held[j] = ok
			close(c.landed[j])
			next = j + 1
		}
	}
	for j, p := range c.pieces {
		if !c.room(ctx) {
			return ctx.Err() // nil when the committer failed; it reports why
		}
		ev := PushEvent{Copy: c.i, Provider: c.ds.Provider, Piece: p.Index, Total: p.CARBytes}
		fail := func(err error) error {
			ev.Err, ev.Final = err, true
			c.progress(ev)
			return err
		}
		has, err := c.s.Has(ctx, c.ds, p.PieceCID)
		if err != nil {
			return fail(err)
		}
		if has {
			// Commit returns a held piece's id without going on chain.
			ids, err := c.s.Commit(ctx, c.ds, []string{p.PieceCID}, c.meta)
			if err != nil {
				return fail(err)
			}
			landed(j, true)
			if err := c.record(p, ids[0]); err != nil {
				return err
			}
			ev.Done, ev.Skipped, ev.Final = p.CARBytes, true, true
			c.progress(ev)
			continue
		}
		err = c.stage(ctx, j, p, &ev)
		landed(j, err == nil)
		if err != nil {
			return fail(err)
		}
		ev.Done, ev.Staged = p.CARBytes, true
		c.progress(ev)
		ev.Staged = false
		c.mu.Lock()
		c.queue = append(c.queue, staged{p: p, at: time.Now(), ev: ev})
		c.cond.Broadcast()
		c.mu.Unlock()
	}
	return nil
}

// room waits while the copy has sent stageAhead's worth ahead of what is
// added. It reports false if the push should stop.
func (c *copyPush) room(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.failed && ctx.Err() == nil && len(c.queue) > 0 && time.Since(c.queue[0].at) >= stageAhead {
		c.cond.Wait()
	}
	return !c.failed && ctx.Err() == nil
}

// stage gets piece j to the copy's provider: pulled from the first copy's
// provider when that holds it, else uploaded from here.
func (c *copyPush) stage(ctx context.Context, j int, p archive.Piece, ev *PushEvent) error {
	if c.landed != nil && c.i > 0 {
		select {
		case <-c.landed[j]:
		case <-ctx.Done():
			return ctx.Err()
		}
		if c.held[j] {
			err := c.s.Pull(ctx, c.ds, c.first, p.PieceCID)
			if err == nil {
				ev.Pulled, ev.From = true, c.first.Provider
				return nil
			}
			if ctx.Err() != nil {
				return err
			}
			c.progress(PushEvent{Copy: ev.Copy, Provider: ev.Provider, Piece: ev.Piece, Total: ev.Total, From: c.first.Provider, PullErr: err})
		}
	}
	return c.upload(ctx, p, ev)
}

// upload sends the piece's CAR from this machine.
func (c *copyPush) upload(ctx context.Context, p archive.Piece, ev *PushEvent) error {
	f, err := os.Open(p.Path)
	if err != nil {
		return fmt.Errorf("piece %d: %w", p.Index, err)
	}
	defer f.Close()
	sent := *ev
	r := &counter{r: f, report: func(done int64) {
		sent.Done = done
		c.progress(sent)
	}}
	ev.Pulled, ev.From = false, ""
	return c.s.Stage(ctx, c.ds, p.PieceCID, r, p.CARBytes)
}

// commitLoop adds staged pieces on chain, all those waiting at once (up to
// commitBatch), until the sender is done and nothing waits.
func (c *copyPush) commitLoop(ctx context.Context) error {
	for {
		c.mu.Lock()
		for len(c.queue) == 0 && c.sending && ctx.Err() == nil {
			c.cond.Wait()
		}
		if ctx.Err() != nil || len(c.queue) == 0 {
			c.mu.Unlock()
			return ctx.Err()
		}
		batch := slices.Clone(c.queue[:min(len(c.queue), commitBatch)])
		c.mu.Unlock()

		ids, err := c.commit(ctx, batch)
		if err == nil {
			for k, st := range batch {
				if err = c.record(st.p, ids[k]); err != nil {
					break
				}
				st.ev.Done, st.ev.Final = st.p.CARBytes, true
				c.progress(st.ev)
			}
		} else {
			ev := batch[0].ev
			ev.Err, ev.Final = err, true
			c.progress(ev)
		}
		c.mu.Lock()
		if err != nil {
			c.failed = true
		} else {
			c.queue = c.queue[len(batch):]
		}
		c.cond.Broadcast()
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// commit adds the batch, sending again any piece the provider dropped while
// it waited.
func (c *copyPush) commit(ctx context.Context, batch []staged) ([]string, error) {
	cids := make([]string, len(batch))
	for k, st := range batch {
		cids[k] = st.p.PieceCID
	}
	for attempt := 1; ; attempt++ {
		ids, err := c.s.Commit(ctx, c.ds, cids, c.meta)
		var gone *GoneError
		if !errors.As(err, &gone) || attempt == 3 {
			return ids, err
		}
		for k := range batch {
			st := &batch[k]
			if !slices.Contains(gone.PieceCIDs, st.p.PieceCID) {
				continue
			}
			ev := st.ev
			ev.Dropped = true
			c.progress(ev)
			if err := c.upload(ctx, st.p, &st.ev); err != nil {
				return nil, err
			}
		}
	}
}

// counter reports bytes read every 256 KiB, and once more at EOF.
type counter struct {
	r      io.Reader
	n, at  int64
	report func(int64)
}

func (c *counter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	if c.n-c.at >= 256<<10 || (err == io.EOF && c.n > c.at) {
		c.at = c.n
		c.report(c.n)
	}
	return n, err
}
