package dream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
	_ "github.com/mattn/go-sqlite3"
)

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
