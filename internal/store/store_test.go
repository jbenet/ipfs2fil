package store

import (
	"math"
	"testing"
)

func TestEstimate(t *testing.T) {
	cases := []struct {
		bytes          int64
		pieces, copies int
		monthly        float64
	}{
		{10 << 30, 10, 3, 0.36}, // floor applies to each data set
		{1 << 40, 1000, 3, 7.5},
		{10 << 40, 10000, 3, 75},
	}
	for _, c := range cases {
		got := Estimate(c.bytes, c.pieces, c.copies)
		if math.Abs(got.Monthly-c.monthly) > 0.005 {
			t.Errorf("%d bytes: monthly %.3f, want %.2f", c.bytes, got.Monthly, c.monthly)
		}
	}
}
