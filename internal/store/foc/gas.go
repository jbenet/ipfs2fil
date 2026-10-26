package foc

import (
	"context"
	"math/big"
	"sync"
	"time"
)

// DefaultMaxBaseFee is the base fee, in attoFIL per gas, at or above which a
// push waits before anything goes on chain. The network floor is 100, so this
// is 10,000 times it. PDP transactions burn a lot of gas, and providers pay
// for the ones they send for us, so a push waits out congestion rather than
// paying many times the usual cost.
const DefaultMaxBaseFee = 1_000_000

// gasGate holds on-chain steps while the base fee is at or above max.
type gasGate struct {
	max   *big.Int // nil: never wait
	read  func(ctx context.Context) (*big.Int, error)
	every time.Duration
	// report is called when the gate starts holding (paused) and when it
	// lets steps through again.
	report func(fee *big.Int, paused bool)

	mu     sync.Mutex
	paused bool
}

// wait returns once the base fee is below max, or ctx ends. If the base fee
// cannot be read, wait lets the step through: the transaction then fails or
// succeeds on its own.
func (g *gasGate) wait(ctx context.Context) error {
	if g == nil || g.max == nil {
		return nil
	}
	for {
		fee, err := g.read(ctx)
		if err != nil || fee == nil || fee.Cmp(g.max) < 0 {
			if err == nil && fee != nil {
				g.set(fee, false)
			}
			return nil
		}
		g.set(fee, true)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.every):
		}
	}
}

func (g *gasGate) set(fee *big.Int, paused bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused == paused {
		return
	}
	g.paused = paused
	if g.report != nil {
		g.report(fee, paused)
	}
}
