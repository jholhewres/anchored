package memory

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
)

// TestVectorCacheScore_MatchesCosine proves the memoized scan returns scores
// bit-equivalent to the original per-vector QuantizeFloat32 + CosineSimilarity,
// so the optimization changes performance, not ranking.
func TestVectorCacheScore_MatchesCosine(t *testing.T) {
	c := NewVectorCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
	vecs := map[string][]float32{
		"a": {0.1, 0.2, 0.3, 0.4},
		"b": {0.9, 0.1, 0.0, 0.2},
		"c": {-0.3, 0.5, 0.2, 0.1},
	}
	for id, v := range vecs {
		c.Put(id, v)
	}
	query := []float32{0.2, 0.2, 0.25, 0.35}
	qn := VectorNorm(query)

	got := c.Score(query, qn, -1, 0) // no threshold/topK cap: score everything
	if len(got) != len(vecs) {
		t.Fatalf("scored %d, want %d", len(got), len(vecs))
	}
	for _, s := range got {
		want := QuantizeFloat32(vecs[s.ID]).CosineSimilarity(query, qn)
		if math.Abs(s.Score-want) > 1e-12 {
			t.Fatalf("score for %s: got %v want %v (delta %g)", s.ID, s.Score, want, math.Abs(s.Score-want))
		}
	}
	// Score must be sorted descending.
	for i := 1; i < len(got); i++ {
		if got[i-1].Score < got[i].Score {
			t.Fatalf("Score not sorted descending: %+v", got)
		}
	}
}

// TestStoreSearch_CrashSafeOnPunctuation: a raw query with FTS5 metacharacters
// must not error — Search retries with a sanitized OR-of-tokens and still finds
// the seeded memory.
func TestStoreSearch_CrashSafeOnPunctuation(t *testing.T) {
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Save(ctx, Memory{ID: "m1", Category: "decision",
		Content: "we chose Postgres for durable team memory storage"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	for _, q := range []string{
		`postgres: storage, durable!`, // punctuation that breaks raw FTS5
		`"unbalanced quote`,
		`team AND/OR memory (notes)`,
	} {
		res, err := st.Search(ctx, q, SearchOptions{MaxResults: 5})
		if err != nil {
			t.Fatalf("Search(%q) must be crash-safe, got error: %v", q, err)
		}
		_ = res // result set may vary; the guarantee under test is "no error"
	}

	// And a sanitizable query still matches.
	res, err := st.Search(ctx, `postgres, storage`, SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("sanitized query should still find the seeded memory")
	}
}
