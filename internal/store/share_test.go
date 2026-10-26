package store

import "testing"

func TestSharePlansBySpeed(t *testing.T) {
	sizes := make([]int64, 20)
	for j := range sizes {
		sizes[j] = 100
	}
	sh := newShare(sizes)
	sh.todo = []int{0, 1, 2, 3}
	sh.up[0] = &uploader{active: true, piece: -1}
	sh.up[1] = &uploader{active: true, piece: -1}
	if k := sh.pick(1); k != 0 {
		t.Errorf("unmeasured: pick = %d, want 0", k)
	}

	// Copy 0 sends 100 B/s and copy 1 10 B/s: copy 0 finishes all four
	// before copy 1 would finish one, so copy 1 waits and pulls them.
	sh.up[0].speed, sh.up[1].speed = 100, 10
	if k := sh.pick(1); k != -1 {
		t.Errorf("slow copy: pick = %d, want -1", k)
	}
	if k := sh.pick(0); k != 0 {
		t.Errorf("fast copy: pick = %d, want 0", k)
	}

	// With 20 left, copy 1 is worth starting on the piece copy 0 would
	// reach only after 10s.
	sh.todo = nil
	for j := range sizes {
		sh.todo = append(sh.todo, j)
	}
	if k := sh.pick(1); k != 9 {
		t.Errorf("slow copy, many pieces: pick = %d, want 9", k)
	}

	// Equal speeds: whichever copy asks takes the next piece, so tied
	// copies never all wait on each other.
	sh.up[1].speed = 100
	if k0, k1 := sh.pick(0), sh.pick(1); k0 != 0 || k1 != 0 {
		t.Errorf("tie: picks %d, %d; want 0, 0", k0, k1)
	}

	// A copy that is not waiting or sending is left out of the plan.
	sh.up[0].active = false
	sh.up[1].speed = 10
	if k := sh.pick(1); k != 0 {
		t.Errorf("only copy: pick = %d, want 0", k)
	}
}
