package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
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
	// Pulled is set when the copy's provider fetched the piece from another
	// copy's provider (From) instead of from this machine.
	Pulled bool
	From   string
	// Pulling marks the non-final event sent when the copy's provider starts
	// fetching the piece from another copy's provider (From). A fetch reports
	// nothing more until it ends, which can take many minutes.
	Pulling bool
	// PullErr says why a provider could not fetch the piece from another
	// copy's provider, on a non-final event; the piece is then uploaded from
	// here.
	PullErr error
	// Slow marks a non-final event: a fetch from another copy's provider
	// (From) passed SlowAfter, and the provider, which turns away an upload
	// of the piece while it fetches, is waited for a while longer.
	Slow      bool
	SlowAfter time.Duration
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
// Each piece leaves this machine once. The copies take turns sending the
// pieces no copy holds, so the uploads to their providers run at the same
// time, and each copy's provider pulls the pieces others sent from the
// provider that got them, falling back to an upload from here if the pull
// fails. A piece a copy already holds is pulled from that copy's provider.
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

	cs := make([]*copyPush, len(sets))
	for i, ds := range sets {
		cs[i] = &copyPush{s: s, i: i, ds: ds, sets: sets, pieces: pieces, progress: progress,
			meta: map[string]string{"ipfsRootCID": m.Root},
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
	}
	errs := make([]error, len(sets))
	fail := func(i int, err error) {
		errs[i] = fmt.Errorf("copy %d on provider %s: %w", i+1, sets[i].Provider, err)
	}

	// First find what each copy holds already, so pieces some copy holds are
	// pulled from it rather than sent from here again.
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Go(func() {
			if err := c.scan(ctx); err != nil {
				fail(i, err)
			}
		})
	}
	wg.Wait()
	sizes := make([]int64, len(pieces))
	for j, p := range pieces {
		sizes[j] = p.CARBytes
	}
	sh := newShare(sizes)
	stop := context.AfterFunc(ctx, sh.wake)
	defer stop()
	for j := range pieces {
		for i, c := range cs {
			if errs[i] == nil && c.has[j] {
				sh.from[j] = i
				break
			}
		}
		if sh.from[j] < 0 {
			sh.todo = append(sh.todo, j)
		}
	}
	for i, c := range cs {
		if errs[i] != nil {
			continue
		}
		c.sh = sh
		wg.Go(func() {
			if err := c.run(ctx); err != nil {
				fail(i, err)
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

// share hands the pieces no copy holds to the copies to send from this
// machine, so each copy uploads to its provider at the same time as the
// others. It measures how fast each copy's uploads go and gives a copy only
// the pieces it would finish before the others could, so a slow provider
// sends few pieces and pulls the rest. It tells the copies when a piece
// reaches a provider they can pull it from.
type share struct {
	mu     sync.Mutex
	cond   *sync.Cond
	size   []int64 // per piece: CAR bytes
	todo   []int   // pieces no copy holds or is sending, in order
	from   []int   // per piece: the copy whose provider holds it, or -1
	owner  []int   // per piece: the copy sending it from here, or -1
	flying int     // pieces being sent from here
	up     map[int]*uploader
	idle   map[int]int       // per copy: its pullers waiting for a piece
	busy   map[int]time.Time // per copy: since when all its pullers are busy
	now    func() time.Time
}

// uploader is one copy's sending from here, while it waits for or sends a
// piece.
type uploader struct {
	active bool      // waiting in take or sending; planned for
	speed  float64   // bytes per second over its earlier pieces, 0 until one is sent
	piece  int       // the piece being sent, or -1 while waiting for one
	done   int64     // bytes of it sent
	start  time.Time // when it started
}

func newShare(sizes []int64) *share {
	sh := &share{size: sizes, from: make([]int, len(sizes)), owner: make([]int, len(sizes)),
		up: map[int]*uploader{}, idle: map[int]int{}, busy: map[int]time.Time{}, now: time.Now}
	sh.cond = sync.NewCond(&sh.mu)
	for j := range sizes {
		sh.from[j], sh.owner[j] = -1, -1
	}
	return sh
}

func (sh *share) wake() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.cond.Broadcast()
}

// take gives copy i a piece to send from here. It waits while the other
// copies would send all that is left sooner, or send the last pieces (a
// failed copy gives its piece back), and reports false once nothing is left
// or stop says so.
func (sh *share) take(i int, stop func() bool) (int, bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	u := sh.up[i]
	if u == nil {
		u = &uploader{piece: -1}
		sh.up[i] = u
	}
	u.active = true
	defer func() {
		if u.piece < 0 {
			u.active = false
			sh.cond.Broadcast()
		}
	}()
	for !stop() {
		if len(sh.todo) == 0 && sh.flying == 0 {
			return 0, false
		}
		if k := sh.pick(i); k >= 0 {
			j := sh.todo[k]
			sh.todo = slices.Delete(sh.todo, k, k+1)
			sh.owner[j] = i
			sh.flying++
			u.piece, u.done, u.start = j, 0, sh.now()
			return j, true
		}
		sh.cond.Wait()
	}
	return 0, false
}

// pick plans who sends the pieces left, each to the copy that would finish
// it first given what it is sending now, and returns the index in todo of
// the first piece the plan gives copy i, or -1 if it gives copy i none.
// Copies not yet measured are planned at the others' average speed; with
// none measured, copy i takes the next piece.
func (sh *share) pick(i int) int {
	if len(sh.todo) == 0 {
		return -1
	}
	var sum float64
	var n int
	for _, u := range sh.up {
		if u.active && u.speed > 0 {
			sum += u.speed
			n++
		}
	}
	if n == 0 {
		return 0
	}
	avg := sum / float64(n)
	now := sh.now()
	type lane struct {
		copy  int
		speed float64
		free  float64 // seconds from now until it can start another piece
	}
	// Copy i plans first, so it takes a piece on a tie: copies tied with
	// each other then never all wait on one another.
	ks := slices.Sorted(maps.Keys(sh.up))
	slices.SortStableFunc(ks, func(a, b int) int { return cmp.Compare(b2i(b == i), b2i(a == i)) })
	var lanes []lane
	for _, k := range ks {
		u := sh.up[k]
		if !u.active {
			continue
		}
		l := lane{copy: k, speed: u.speed}
		if l.speed == 0 {
			l.speed = avg
		}
		if u.piece >= 0 {
			speed := l.speed
			// A piece going slower than before shows the link now.
			if el := now.Sub(u.start).Seconds(); el >= 2 && u.done > 0 {
				speed = min(speed, float64(u.done)/el)
			}
			l.free = float64(sh.size[u.piece]-u.done) / speed
		}
		lanes = append(lanes, l)
	}
	for k, j := range sh.todo {
		best := 0
		for m := range lanes {
			if lanes[m].free+float64(sh.size[j])/lanes[m].speed < lanes[best].free+float64(sh.size[j])/lanes[best].speed {
				best = m
			}
		}
		if lanes[best].copy == i {
			return k
		}
		lanes[best].free += float64(sh.size[j]) / lanes[best].speed
	}
	return -1
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// note records bytes of copy i's current piece sent so far.
func (sh *share) note(i int, done int64) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if u := sh.up[i]; u != nil {
		u.done = done
	}
}

// sent records that copy i's provider holds piece j, or with ok false that
// copy i failed to send it, so another copy takes it.
func (sh *share) sent(i, j int, ok bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.flying--
	u := sh.up[i]
	if ok {
		sh.from[j] = i
		if el := max(sh.now().Sub(u.start).Seconds(), 0.001); el > 0 {
			if v := float64(sh.size[j]) / el; u.speed == 0 {
				u.speed = v
			} else {
				u.speed = (u.speed + v) / 2
			}
		}
	} else {
		sh.owner[j] = -1
		sh.todo = append([]int{j}, sh.todo...)
	}
	u.piece, u.active = -1, false
	sh.cond.Broadcast()
}

// next waits until one of copy i's wanted pieces has reached another copy's
// provider and returns it with that copy. Pieces copy i itself sends drop out
// of wanted. It reports false once wanted is empty or stop says so.
func (sh *share) next(i int, wanted *[]int, stop func() bool) (j, from int, ok bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	sh.idle[i]++
	defer func() {
		if sh.idle[i]--; sh.idle[i] == 0 {
			sh.busy[i] = sh.now()
		}
		sh.cond.Broadcast()
	}()
	for {
		*wanted = slices.DeleteFunc(*wanted, func(j int) bool { return sh.owner[j] == i || sh.from[j] == i })
		if len(*wanted) == 0 || stop() {
			return 0, 0, false
		}
		for k, j := range *wanted {
			if f := sh.from[j]; f >= 0 {
				*wanted = slices.Delete(*wanted, k, k+1)
				return j, f, true
			}
		}
		sh.cond.Wait()
	}
}

// helpAfter is how long all of a copy's pullers must have been busy, none
// finishing, before its uploader sends a piece they would pull.
var helpAfter = time.Minute

// help gives copy i's uploader, once it has nothing of its own to send, a
// wanted piece that another copy's provider holds once all of copy i's
// pullers have been busy for helpAfter, so the piece is sent from here rather
// than wait for slow pulls. Pulls that keep finishing keep the pieces to
// them, so each leaves this machine once. It reports false once wanted is
// empty or stop says so.
func (sh *share) help(i int, wanted *[]int, stop func() bool) (int, bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for {
		*wanted = slices.DeleteFunc(*wanted, func(j int) bool { return sh.owner[j] == i || sh.from[j] == i })
		if len(*wanted) == 0 || stop() {
			return 0, false
		}
		if sh.idle[i] == 0 {
			if sh.busy[i].IsZero() { // the pullers have not started yet
				sh.busy[i] = sh.now()
			}
			if left := helpAfter - sh.now().Sub(sh.busy[i]); left > 0 {
				time.AfterFunc(left, sh.wake)
			} else {
				for k, j := range *wanted {
					if sh.from[j] >= 0 {
						*wanted = slices.Delete(*wanted, k, k+1)
						return j, true
					}
				}
			}
		}
		sh.cond.Wait()
	}
}

// copyPush stores the pieces in one copy: it sends its share of the pieces
// from here and has its provider pull the rest from the other copies'
// providers, while a committer adds the staged pieces on chain in batches.
type copyPush struct {
	s        Store
	i        int
	ds       DataSet
	sets     []DataSet
	pieces   []archive.Piece
	meta     map[string]string
	progress func(PushEvent)
	record   func(p archive.Piece, id string) error
	has      []bool // per piece: held when the push started
	sh       *share
	wanted   []int // pieces the copy lacks that nobody here is getting; guarded by sh.mu

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []staged // staged and waiting to be added, oldest first
	sending bool
	failed  bool // the committer stopped
	halted  atomic.Bool
}

type staged struct {
	p  archive.Piece
	at time.Time
	ev PushEvent
}

// scan finds the pieces the copy holds and records them as skipped.
func (c *copyPush) scan(ctx context.Context) error {
	c.has = make([]bool, len(c.pieces))
	for j, p := range c.pieces {
		ev := PushEvent{Copy: c.i, Provider: c.ds.Provider, Piece: p.Index, Total: p.CARBytes}
		has, err := c.s.Has(ctx, c.ds, p.PieceCID)
		if err == nil && has {
			// Commit returns a held piece's id without going on chain.
			var ids []string
			if ids, err = c.s.Commit(ctx, c.ds, []string{p.PieceCID}, c.meta); err == nil {
				err = c.record(p, ids[0])
			}
		}
		if err != nil {
			ev.Err, ev.Final = err, true
			c.progress(ev)
			return err
		}
		if has {
			c.has[j] = true
			ev.Done, ev.Skipped, ev.Final = p.CARBytes, true, true
			c.progress(ev)
		}
	}
	return nil
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
	for j := range c.pieces {
		if !c.has[j] {
			c.wanted = append(c.wanted, j)
		}
	}
	committed := make(chan error, 1)
	go func() { committed <- c.commitLoop(ctx) }()
	var upErr, pullErr error
	var wg sync.WaitGroup
	wg.Go(func() { upErr = c.uploads(ctx) })
	wg.Go(func() { pullErr = c.pulls(ctx) })
	wg.Wait()
	c.mu.Lock()
	c.sending = false
	c.cond.Broadcast()
	c.mu.Unlock()
	// What was staged before a send failed is still added.
	commitErr := <-committed
	if commitErr != nil {
		return commitErr
	}
	return errors.Join(upErr, pullErr)
}

// halt stops the copy's sending after an error.
func (c *copyPush) halt() {
	c.halted.Store(true)
	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
	c.sh.wake()
}

func (c *copyPush) stopped(ctx context.Context) func() bool {
	return func() bool { return c.halted.Load() || ctx.Err() != nil }
}

// uploads sends pieces no copy holds from here, taking them one at a time
// from the share until none are left.
func (c *copyPush) uploads(ctx context.Context) error {
	for c.room(ctx) {
		j, ok := c.sh.take(c.i, c.stopped(ctx))
		if !ok {
			break
		}
		p := c.pieces[j]
		ev := PushEvent{Copy: c.i, Provider: c.ds.Provider, Piece: p.Index, Total: p.CARBytes}
		err := c.upload(ctx, p, &ev, func(n int64) { c.sh.note(c.i, n) })
		c.sh.sent(c.i, j, err == nil)
		if err != nil {
			return c.fail(ev, err)
		}
		c.queue1(p, ev)
	}
	// With nothing left that no copy holds, send from here the pieces this
	// copy still lacks while its pullers are busy: a provider fetching from
	// another can take far longer than an upload from here.
	for c.room(ctx) {
		j, ok := c.sh.help(c.i, &c.wanted, c.stopped(ctx))
		if !ok {
			break
		}
		p := c.pieces[j]
		ev := PushEvent{Copy: c.i, Provider: c.ds.Provider, Piece: p.Index, Total: p.CARBytes}
		if err := c.upload(ctx, p, &ev, nil); err != nil {
			return c.fail(ev, err)
		}
		c.queue1(p, ev)
	}
	return ctx.Err() // nil when the committer failed; it reports why
}

// pullTimeout is how long a provider may take to fetch a piece of size bytes
// from another before it is sent from here instead: a quarter hour, plus the
// time at 2 MiB/s.
var pullTimeout = func(size int64) time.Duration {
	return 15*time.Minute + time.Duration(size/(2<<20))*time.Second
}

// Awaiter is a Store whose providers keep fetching a piece after Pull stops
// waiting for it, and turn away an upload of that piece until they finish,
// as FOC providers do. Await waits until ds's provider holds the piece ready
// to add.
type Awaiter interface {
	Await(ctx context.Context, ds DataSet, pieceCID string) error
}

// slowPullWait is how much longer a pull that passed pullTimeout is waited
// for on an Awaiter, before the piece is left for the next push.
var slowPullWait = func(size int64) time.Duration { return 2 * pullTimeout(size) }

// pullsAtOnce is how many pieces a copy's provider fetches from other
// providers at the same time.
const pullsAtOnce = 4

// pulls has the copy's provider fetch each piece it lacks from a provider
// that holds it, once one does, up to pullsAtOnce at a time, and sends a
// piece from here if fetching it fails.
func (c *copyPush) pulls(ctx context.Context) error {
	errs := make([]error, pullsAtOnce)
	var wg sync.WaitGroup
	for k := range errs {
		wg.Go(func() { errs[k] = c.puller(ctx, &c.wanted) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (c *copyPush) puller(ctx context.Context, wanted *[]int) error {
	var late []error // pieces the provider was still fetching when the wait ran out
	for c.room(ctx) {
		j, from, ok := c.sh.next(c.i, wanted, c.stopped(ctx))
		if !ok {
			break
		}
		p := c.pieces[j]
		ev := PushEvent{Copy: c.i, Provider: c.ds.Provider, Piece: p.Index, Total: p.CARBytes}
		src := c.sets[from]
		c.progress(PushEvent{Copy: ev.Copy, Provider: ev.Provider, Piece: ev.Piece, Total: ev.Total, From: src.Provider, Pulling: true})
		d := pullTimeout(p.CARBytes)
		pctx, cancel := context.WithTimeout(ctx, d)
		err := c.s.Pull(pctx, c.ds, src, p.PieceCID)
		cancel()
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			if a, ok := c.s.(Awaiter); ok {
				// The provider is still fetching it and would turn an
				// upload from here away, so wait for the fetch instead.
				c.progress(PushEvent{Copy: ev.Copy, Provider: ev.Provider, Piece: ev.Piece, Total: ev.Total, From: src.Provider, Pulling: true, Slow: true, SlowAfter: d})
				w := slowPullWait(p.CARBytes)
				actx, cancel := context.WithTimeout(ctx, w)
				err = a.Await(actx, c.ds, p.PieceCID)
				cancel()
				if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
					ev.From = src.Provider
					ev.Err, ev.Final = fmt.Errorf("provider %s was still copying it from provider %s after %s; run push again later to add it", c.ds.Provider, src.Provider, d+w), true
					c.progress(ev)
					late = append(late, fmt.Errorf("piece %d: %w", p.Index, ev.Err))
					continue
				}
			} else {
				err = fmt.Errorf("gave up after %s", d)
			}
		}
		if err == nil {
			ev.Pulled, ev.From = true, src.Provider
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.progress(PushEvent{Copy: ev.Copy, Provider: ev.Provider, Piece: ev.Piece, Total: ev.Total, From: src.Provider, PullErr: err})
			if err := c.upload(ctx, p, &ev, nil); err != nil {
				return c.fail(ev, err)
			}
		}
		c.queue1(p, ev)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(late...)
}

func (c *copyPush) fail(ev PushEvent, err error) error {
	ev.Err, ev.Final = err, true
	c.progress(ev)
	c.halt()
	return err
}

// queue1 reports a staged piece and queues it to be added.
func (c *copyPush) queue1(p archive.Piece, ev PushEvent) {
	ev.Done, ev.Staged = p.CARBytes, true
	c.progress(ev)
	ev.Staged = false
	c.mu.Lock()
	c.queue = append(c.queue, staged{p: p, at: time.Now(), ev: ev})
	c.cond.Broadcast()
	c.mu.Unlock()
}

// room waits while the copy has sent stageAhead's worth ahead of what is
// added. It reports false if the push should stop.
func (c *copyPush) room(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for !c.failed && !c.halted.Load() && ctx.Err() == nil && len(c.queue) > 0 && time.Since(c.queue[0].at) >= stageAhead {
		c.cond.Wait()
	}
	return !c.failed && !c.halted.Load() && ctx.Err() == nil
}

// upload sends the piece's CAR from this machine, telling note (if set) the
// bytes sent so far.
func (c *copyPush) upload(ctx context.Context, p archive.Piece, ev *PushEvent, note func(int64)) error {
	f, err := os.Open(p.Path)
	if err != nil {
		return fmt.Errorf("piece %d: %w", p.Index, err)
	}
	defer f.Close()
	sent := *ev
	r := &counter{r: f, report: func(done int64) {
		sent.Done = done
		c.progress(sent)
		if note != nil {
			note(done)
		}
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
		if err != nil {
			c.halt()
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
			if err := c.upload(ctx, st.p, &st.ev, nil); err != nil {
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
