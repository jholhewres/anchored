package memory

import (
	"testing"
	"time"
)

func TestDiversifyByOrigin_CapsPerSession(t *testing.T) {
	sid := "sess-A"
	results := make([]SearchResult, 0, 6)
	for i := 0; i < 6; i++ {
		results = append(results, SearchResult{
			Memory: Memory{ID: string(rune('a' + i)), SourceID: ptr(sid)},
			Score:  float64(6 - i),
		})
	}

	out := diversifyByOrigin(results, 3)
	if len(out) != 3 {
		t.Fatalf("expected 3 kept for one session with cap 3, got %d", len(out))
	}
	// Score order preserved → the three highest-scoring survive.
	for i := 0; i < 3; i++ {
		if out[i].Memory.ID != results[i].Memory.ID {
			t.Fatalf("expected highest-scoring kept in order; out[%d]=%q want %q", i, out[i].Memory.ID, results[i].Memory.ID)
		}
	}
}

func TestDiversifyByOrigin_MixedOriginsUntouchedUnderCap(t *testing.T) {
	results := []SearchResult{
		{Memory: Memory{ID: "1", SourceID: ptr("s1")}, Score: 5},
		{Memory: Memory{ID: "2", SourceID: ptr("s2")}, Score: 4},
		{Memory: Memory{ID: "3", SourceID: ptr("s3")}, Score: 3},
	}
	out := diversifyByOrigin(results, 3)
	if len(out) != 3 {
		t.Fatalf("distinct origins under cap must all survive, got %d", len(out))
	}
}

func TestDiversifyByOrigin_FallsBackToDayWhenNoSession(t *testing.T) {
	day1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 2, 10, 0, 0, 0, time.UTC)
	results := []SearchResult{
		{Memory: Memory{ID: "a", CreatedAt: day1}, Score: 6},
		{Memory: Memory{ID: "b", CreatedAt: day1}, Score: 5},
		{Memory: Memory{ID: "c", CreatedAt: day1}, Score: 4}, // 3rd of day1 → dropped at cap 2
		{Memory: Memory{ID: "d", CreatedAt: day2}, Score: 3}, // different day → kept
	}
	out := diversifyByOrigin(results, 2)
	if len(out) != 3 {
		t.Fatalf("expected 2 from day1 + 1 from day2 = 3, got %d", len(out))
	}
	// The day2 memory must survive — memories without a session are not all
	// collapsed into a single origin.
	found := false
	for _, r := range out {
		if r.Memory.ID == "d" {
			found = true
		}
	}
	if !found {
		t.Fatal("day2 memory was wrongly collapsed with day1")
	}
}
