package dream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
	_ "github.com/mattn/go-sqlite3"
)

func setupConsolidatorTestDB(t *testing.T) *sql.DB {
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

func TestConsolidate_ExactDuplicate_SoftDeletes(t *testing.T) {
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	report := &DreamReport{
		Actions: []DreamAction{
			{ID: "a1", MemoryID: "mem-1", ActionType: "dedup", Confidence: 1.0, Reason: "exact hash match"},
		},
	}

	result, err := c.Consolidate(context.Background(), report, DreamConfigForAggressiveness("moderate"))
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 1 {
		t.Errorf("expected 1 soft delete, got %d", result.SoftDeleted)
	}
}

func TestConsolidate_Contradiction_FlagsOnly(t *testing.T) {
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	report := &DreamReport{
		Actions: []DreamAction{
			{ID: "a1", MemoryID: "mem-1", ActionType: "contradiction", Confidence: 0.6, Reason: "negation"},
		},
	}

	result, err := c.Consolidate(context.Background(), report, DreamConfigForAggressiveness("aggressive"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Flagged != 1 {
		t.Errorf("expected 1 flagged, got %d", result.Flagged)
	}
	if result.SoftDeleted != 0 {
		t.Errorf("expected 0 deleted, got %d", result.SoftDeleted)
	}
}

func TestConsolidate_RespectsMaxDeletions(t *testing.T) {
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	actions := make([]DreamAction, 10)
	for i := range actions {
		actions[i] = DreamAction{
			ID:         fmt.Sprintf("a%d", i),
			MemoryID:   fmt.Sprintf("mem-%d", i),
			ActionType: "dedup",
			Confidence: 1.0,
		}
	}

	report := &DreamReport{Actions: actions}
	cfg := DreamConfigForAggressiveness("moderate")
	cfg.MaxDeletionsPerRun = 5

	result, err := c.Consolidate(context.Background(), report, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 5 {
		t.Errorf("expected 5 deletions, got %d", result.SoftDeleted)
	}
	if result.Skipped != 5 {
		t.Errorf("expected 5 skipped, got %d", result.Skipped)
	}
}

func TestConsolidate_ConservativeSkipsAll(t *testing.T) {
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	report := &DreamReport{
		Actions: []DreamAction{
			{ID: "a1", MemoryID: "mem-1", ActionType: "dedup", Confidence: 0.95, Reason: "near dup"},
		},
	}

	result, err := c.Consolidate(context.Background(), report, DreamConfigForAggressiveness("conservative"))
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 0 {
		t.Errorf("conservative should not delete, got %d", result.SoftDeleted)
	}
}

func TestDreamConfig_Levels(t *testing.T) {
	conservative := DreamConfigForAggressiveness("conservative")
	moderate := DreamConfigForAggressiveness("moderate")
	aggressive := DreamConfigForAggressiveness("aggressive")

	if conservative.DedupThreshold <= moderate.DedupThreshold {
		t.Errorf("conservative threshold (%.2f) should be > moderate (%.2f)", conservative.DedupThreshold, moderate.DedupThreshold)
	}
	if moderate.DedupThreshold <= aggressive.DedupThreshold {
		t.Errorf("moderate threshold (%.2f) should be > aggressive (%.2f)", moderate.DedupThreshold, aggressive.DedupThreshold)
	}
}
