package foc

import (
	"context"
	"math/big"
	"sync"
	"time"
)

// DefaultMaxBaseFee is the base fee, in attoFIL per gas, at or above which a
// push holds its on-chain steps (uploads carry on). The network floor is 100.
// Busy stretches run at 1-2 million for hours: on 2026-10-26 mainnet sat
// between 1.3 and 2.3 million for over 10 hours, where an addPieces (about
// 300 million gas) costs the provider about 0.0005 FIL, so waiting those out
// only stalls the push. 100 million (0.1 nanoFIL) is 50 times that, about
// 0.03 FIL per addPieces: only a real spike waits.
const DefaultMaxBaseFee = 100_000_000

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
