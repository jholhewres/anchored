package dream

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
	_ "github.com/mattn/go-sqlite3"
)

func float32sToBytes(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		bits := math.Float32bits(v)
		buf[i*4] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := memory.Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAnalyze_EmptyDatabase(t *testing.T) {
	db := setupTestDB(t)
	a := NewAnalyzer(db, nil, DefaultDreamConfig(), nil)

	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalMemories != 0 {
		t.Errorf("expected 0, got %d", report.TotalMemories)
	}
	if len(report.Actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(report.Actions))
	}
}

func TestAnalyze_ExactDuplicates(t *testing.T) {
	db := setupTestDB(t)

	for i := 0; i < 3; i++ {
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO memories (id, content, category, content_hash, created_at) VALUES (?, ?, 'fact', ?, ?)",
			fmt.Sprintf("mem-%d", i), "test content duplicate", "hash123", time.Now().Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}

	a := NewAnalyzer(db, nil, DefaultDreamConfig(), nil)
	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.ExactDupes != 2 {
		t.Errorf("expected 2 exact dupes, got %d", report.ExactDupes)
	}

	dedupActions := 0
	for _, a := range report.Actions {
		if a.ActionType == "dedup" && a.Confidence == 1.0 {
			dedupActions++
		}
	}
	if dedupActions != 2 {
		t.Errorf("expected 2 exact dedup actions, got %d", dedupActions)
	}
}

func TestAnalyze_NoDuplicates(t *testing.T) {
	db := setupTestDB(t)

	contents := []string{"first unique content", "second unique content", "third unique content"}
	for i, c := range contents {
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO memories (id, content, category, content_hash, created_at) VALUES (?, ?, 'fact', ?, ?)",
			fmt.Sprintf("mem-%d", i), c, fmt.Sprintf("hash-%d", i), time.Now())
		if err != nil {
			t.Fatal(err)
		}
	}

	a := NewAnalyzer(db, nil, DefaultDreamConfig(), nil)
	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.ExactDupes != 0 || report.NearDupes != 0 {
		t.Errorf("expected no dupes, got exact=%d near=%d", report.ExactDupes, report.NearDupes)
	}
}

func TestDetectNegation_Positive(t *testing.T) {
	if !detectNegation("uses React", "does not use React") {
		t.Error("expected negation detected")
	}
}

func TestDetectNegation_Negative(t *testing.T) {
	if detectNegation("uses Go", "uses Docker") {
		t.Error("expected no negation detected")
	}
}

func TestDetectAntonyms_Positive(t *testing.T) {
	if !detectAntonyms("feature is enabled", "feature is disabled") {
		t.Error("expected antonyms detected")
	}
}

func TestDetectAntonyms_Negative(t *testing.T) {
	if detectAntonyms("uses Go", "uses Docker") {
		t.Error("expected no antonyms detected")
	}
}

func TestAnalyze_NearDuplicates_CacheFromDB(t *testing.T) {
	db := setupTestDB(t)

	vec1 := []float32{1.0, 0.5, 0.3, 0.2}
	vec2 := []float32{0.95, 0.52, 0.28, 0.21}
	vec3 := []float32{-0.5, 0.8, -0.3, 0.1}
	vecs := [][]float32{vec1, vec2, vec3}

	for i, vec := range vecs {
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO memories (id, content, category, content_hash, embedding, created_at) VALUES (?, ?, 'fact', ?, ?, ?)",
			fmt.Sprintf("mem-%d", i), fmt.Sprintf("unique content %d", i), fmt.Sprintf("hash-%d", i),
			float32sToBytes(vec), time.Now().Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}

	cache := memory.NewVectorCache(nil)
	if err := cache.Load(db); err != nil {
		t.Fatalf("cache load: %v", err)
	}
	if cache.Len() != 3 {
		t.Fatalf("expected 3 cache entries, got %d", cache.Len())
	}

	cfg := DefaultDreamConfig()
	a := NewAnalyzer(db, cache, cfg, nil)
	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.ExactDupes != 0 {
		t.Errorf("expected 0 exact dupes, got %d", report.ExactDupes)
	}
	if report.NearDupes != 1 {
		t.Errorf("expected 1 near-dupe, got %d", report.NearDupes)
	}
}

func TestAnalyze_NearDuplicates(t *testing.T) {
	db := setupTestDB(t)

	// Insert 3 memories with different content (no exact duplicates)
	for i := 0; i < 3; i++ {
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO memories (id, content, category, content_hash, created_at) VALUES (?, ?, 'fact', ?, ?)",
			fmt.Sprintf("mem-%d", i), fmt.Sprintf("unique content %d", i), fmt.Sprintf("hash-%d", i), time.Now().Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create VectorCache with known similar vectors
	cache := memory.NewVectorCache(nil)
	// mem-0 and mem-1 have very similar vectors (cosine ≈ 0.999)
	cache.Put("mem-0", []float32{1.0, 0.5, 0.3, 0.2})
	cache.Put("mem-1", []float32{0.95, 0.52, 0.28, 0.21})
	// mem-2 has a very different vector (cosine with others < 0)
	cache.Put("mem-2", []float32{-0.5, 0.8, -0.3, 0.1})

	cfg := DefaultDreamConfig()
	a := NewAnalyzer(db, cache, cfg, nil)

	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.ExactDupes != 0 {
		t.Errorf("expected 0 exact dupes, got %d", report.ExactDupes)
	}
	if report.NearDupes != 1 {
		t.Errorf("expected 1 near-dupe, got %d", report.NearDupes)
	}

	// Verify the near-dupe action exists with high confidence
	found := false
	for _, action := range report.Actions {
		if action.ActionType == "dedup" && action.Confidence < 1.0 {
			found = true
			if action.Confidence < 0.9 {
				t.Errorf("expected high confidence near-dupe, got %f", action.Confidence)
			}
		}
	}
	if !found {
		t.Error("expected to find a near-duplicate action")
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name   string
		a, b   []float32
		expect float64
	}{
		{"identical", []float32{1, 0, 0}, []float32{1, 0, 0}, 1.0},
		{"orthogonal", []float32{1, 0, 0}, []float32{0, 1, 0}, 0.0},
		{"similar", []float32{1, 1, 0}, []float32{1, 0, 0}, 0.707},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score := cosineSimilarity(tt.a, tt.b)
			if score < tt.expect-0.01 || score > tt.expect+0.01 {
				t.Errorf("cosine(%v, %v) = %f, want ~%f", tt.a, tt.b, score, tt.expect)
			}
		})
	}
}

func TestAnalyze_LargeDataset_NearDuplicatesFound(t *testing.T) {
	db := setupTestDB(t)

	nearVec := []float32{1.0, 0.5, 0.3, 0.2}
	farVec := []float32{-0.5, 0.8, -0.3, 0.1}
	cache := memory.NewVectorCache(nil)

	const total = 100
	const nearCount = 50

	for i := 0; i < total; i++ {
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO memories (id, content, category, content_hash, created_at) VALUES (?, ?, 'fact', ?, ?)",
			fmt.Sprintf("mem-%04d", i), fmt.Sprintf("content %d", i), fmt.Sprintf("hash-%d", i),
			time.Now().Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		vec := farVec
		if i < nearCount {
			vec = nearVec
		}
		cache.Put(fmt.Sprintf("mem-%04d", i), vec)
	}

	cfg := DefaultDreamConfig()
	cfg.MaxPairwiseCompare = 50
	a := NewAnalyzer(db, cache, cfg, nil)

	report, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.NearDupes == 0 {
		t.Error("expected near-dupes to be found with distributed comparisons across 100 entries")
	}
}
