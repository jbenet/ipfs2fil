package foc

import (
	"context"
	"math/big"
	"testing"
	"time"
)

func TestGasGateWaitsOutSpike(t *testing.T) {
	fees := []int64{5_000_000, 2_000_000, 300}
	var reads int
	var reports []bool
	g := &gasGate{
		max:   big.NewInt(DefaultMaxBaseFee),
		every: time.Millisecond,
		read: func(context.Context) (*big.Int, error) {
			f := fees[min(reads, len(fees)-1)]
			reads++
			return big.NewInt(f), nil
		},
		report: func(_ *big.Int, paused bool) { reports = append(reports, paused) },
	}
	if err := g.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads != 3 || len(reports) != 2 || !reports[0] || reports[1] {
		t.Errorf("reads %d, reports %v; want 3 reads, [true false]", reads, reports)
	}
	// Below max from the start: one read, no report.
	reads, reports = 2, nil
	if err := g.wait(context.Background()); err != nil || reads != 3 || len(reports) != 0 {
		t.Errorf("calm network: err %v, reads %d, reports %v", err, reads, reports)
	}
}

func TestGasGateStopsWithContext(t *testing.T) {
	g := &gasGate{max: big.NewInt(10), every: time.Hour,
		read: func(context.Context) (*big.Int, error) { return big.NewInt(11), nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.wait(ctx); err == nil {
		t.Error("wait returned nil after the context ended")
	}
	var off *gasGate
	if err := off.wait(ctx); err != nil {
		t.Errorf("a nil gate waited: %v", err)
	}
}
