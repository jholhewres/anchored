package memory

import (
	"math"
	"testing"
	"time"
)

// decayResult runs a memory through the search-time scoring steps that touch
// age: the lifecycle boosts and the (single) temporal decay.
func decayResult(created time.Time, meta MemoryMetadata) float64 {
	cfg := DefaultHybridSearchConfig()
	h := &HybridSearcher{config: cfg}
	res := []SearchResult{{Memory: Memory{CreatedAt: created, Metadata: meta.ToAny()}, Score: 1.0}}
	res = applyLifecycleBoost(res, time.Now())
	res = h.applyTemporalDecay(res, cfg)
	return res[0].Score
}

// TestAgeDecay_SearchTime guards Feature E's decay: never-used memories fade
// with age at search time; recent use resets the clock; pinned never decays;
// nothing is ever written back.
func TestAgeDecay_SearchTime(t *testing.T) {
	now := time.Now()

	fresh := decayResult(now.Add(-10*24*time.Hour), MemoryMetadata{ScorerVersion: 3})
	aging := decayResult(now.Add(-100*24*time.Hour), MemoryMetadata{ScorerVersion: 3})
	stale := decayResult(now.Add(-200*24*time.Hour), MemoryMetadata{ScorerVersion: 3})

	if !(stale < aging && aging < fresh) {
		t.Fatalf("decay should be monotonic: fresh=%.3f aging=%.3f stale=%.3f", fresh, aging, stale)
	}

	// A recent use resets the decay clock: an old memory used 5 days ago
	// scores like one created 5 days ago.
	used := decayResult(now.Add(-200*24*time.Hour), MemoryMetadata{
		ScorerVersion: 3,
		LastUsedAt:    now.Add(-5 * 24 * time.Hour).Format(time.RFC3339),
	})
	createdThen := decayResult(now.Add(-5*24*time.Hour), MemoryMetadata{ScorerVersion: 3})
	if math.Abs(used-createdThen) > 1e-3 {
		t.Errorf("recent use must reset decay: used=%.3f created-5d-ago=%.3f", used, createdThen)
	}

	// Pinned memories never decay (pinned also gets the 1.5x boost — compare
	// against a pinned fresh memory).
	pinnedOld := decayResult(now.Add(-400*24*time.Hour), MemoryMetadata{ScorerVersion: 3, Pinned: true})
	pinnedNew := decayResult(now.Add(-1*24*time.Hour), MemoryMetadata{ScorerVersion: 3, Pinned: true})
	if pinnedOld != pinnedNew {
		t.Errorf("pinned must not decay: old=%.3f new=%.3f", pinnedOld, pinnedNew)
	}
}
