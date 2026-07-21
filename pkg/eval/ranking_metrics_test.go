package eval

import (
	"math"
	"testing"
)

func TestRankingMetrics(t *testing.T) {
	ranked := []bool{false, true, true}
	if got := reciprocalRank(ranked); got != 0.5 {
		t.Fatalf("MRR=%v, want 0.5", got)
	}
	got := normalizedDiscountedCumulativeGain(ranked, 2, 3)
	want := (1/math.Log2(3) + 1/math.Log2(4)) / (1 + 1/math.Log2(3))
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("nDCG=%v, want %v", got, want)
	}
	if got := normalizedDiscountedCumulativeGain(nil, 0, 5); got != 1 {
		t.Fatalf("empty relevance nDCG=%v, want 1", got)
	}
}
