package dream

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
	_ "github.com/mattn/go-sqlite3"
	"strings"
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

func insertTestMemory(t *testing.T, ctx context.Context, db *sql.DB, id, content string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		"INSERT INTO memories (id, category, content, content_hash, logical_id, current_revision_id) VALUES (?, 'fact', ?, 'hash1', ?1, ?1)",
		id, content)
	if err != nil {
		t.Fatal(err)
	}
}

func insertTestDreamAction(t *testing.T, ctx context.Context, db *sql.DB, id, memoryID, relatedID, actionType string, confidence float64, status string) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		"INSERT INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, status) VALUES (?, 'test-run', ?, ?, ?, ?, 'test', ?)",
		id, memoryID, relatedID, actionType, confidence, status)
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyAction_Dedup_SoftDeletes(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-old", "duplicate content")
	insertTestMemory(t, ctx, db, "mem-new", "duplicate content")
	insertTestDreamAction(t, ctx, db, "act-1", "mem-old", "mem-new", "dedup", 1.0, "proposed")

	result, err := c.ApplyAction(ctx, "act-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" {
		t.Errorf("expected status applied, got %q", result.Status)
	}
	if result.ActionType != "dedup" {
		t.Errorf("expected action_type dedup, got %q", result.ActionType)
	}
	if result.MemoryID != "mem-old" {
		t.Errorf("expected memory_id mem-old, got %q", result.MemoryID)
	}

	var deletedAt *string
	err = db.QueryRowContext(ctx, "SELECT deleted_at FROM memories WHERE id = 'mem-old'").Scan(&deletedAt)
	if err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil {
		t.Error("expected mem-old to be soft-deleted, but deleted_at is nil")
	}

	var actionStatus string
	var appliedAt *string
	err = db.QueryRowContext(ctx, "SELECT status, applied_at FROM dream_actions WHERE id = 'act-1'").Scan(&actionStatus, &appliedAt)
	if err != nil {
		t.Fatal(err)
	}
	if actionStatus != "applied" {
		t.Errorf("expected action status applied, got %q", actionStatus)
	}
	if appliedAt == nil {
		t.Error("expected applied_at to be set, got nil")
	}
}

func TestApplyAction_Contradiction_ReturnsError(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-a", "go is great")
	insertTestMemory(t, ctx, db, "mem-b", "go is terrible")
	insertTestDreamAction(t, ctx, db, "act-cont", "mem-a", "mem-b", "contradiction", 0.6, "proposed")

	_, err := c.ApplyAction(ctx, "act-cont")
	if err == nil {
		t.Fatal("expected error for contradiction action, got nil")
	}
}

func TestApplyAction_NonexistentID_ReturnsError(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	_, err := c.ApplyAction(ctx, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent action, got nil")
	}
}

func TestApplyAction_AlreadyApplied_ReturnsError(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-x", "some content")
	insertTestDreamAction(t, ctx, db, "act-done", "mem-x", "", "dedup", 1.0, "applied")

	_, err := c.ApplyAction(ctx, "act-done")
	if err == nil {
		t.Fatal("expected error for already-applied action, got nil")
	}
}

func TestApplyAction_Supersede_UpdatesMetadata(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-new", "updated content")
	insertTestMemory(t, ctx, db, "mem-old", "old content")
	insertTestDreamAction(t, ctx, db, "act-sup", "mem-new", "mem-old", "supersede", 0.9, "proposed")

	result, err := c.ApplyAction(ctx, "act-sup")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" {
		t.Errorf("status: got %q", result.Status)
	}
	if result.ActionType != "supersede" {
		t.Errorf("action_type: got %q", result.ActionType)
	}

	var meta string
	err = db.QueryRowContext(ctx, "SELECT metadata FROM memories WHERE id = 'mem-new'").Scan(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if meta == "" || meta == "null" {
		t.Fatalf("expected metadata with supersedes, got %q", meta)
	}
	if !containsSubstring(meta, "mem-old") {
		t.Errorf("metadata should reference mem-old, got %q", meta)
	}
}

func TestApplyAction_Supersede_NoRelatedID_Error(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-solo", "content")
	insertTestDreamAction(t, ctx, db, "act-no-rel", "mem-solo", "", "supersede", 0.9, "proposed")

	_, err := c.ApplyAction(ctx, "act-no-rel")
	if err == nil {
		t.Fatal("expected error for supersede without related_memory_id")
	}
}

func TestApplyAction_Merge_ConsolidatesAndSoftDeletes(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-keeper", "combined content")
	insertTestMemory(t, ctx, db, "mem-absorbed", "absorbed content")
	insertTestDreamAction(t, ctx, db, "act-merge", "mem-keeper", "mem-absorbed", "merge", 0.95, "proposed")

	result, err := c.ApplyAction(ctx, "act-merge")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" {
		t.Errorf("status: got %q", result.Status)
	}

	var meta string
	err = db.QueryRowContext(ctx, "SELECT metadata FROM memories WHERE id = 'mem-keeper'").Scan(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstring(meta, "mem-absorbed") {
		t.Errorf("metadata should reference mem-absorbed in consolidates, got %q", meta)
	}

	var deletedAt *string
	err = db.QueryRowContext(ctx, "SELECT deleted_at FROM memories WHERE id = 'mem-absorbed'").Scan(&deletedAt)
	if err != nil {
		t.Fatal(err)
	}
	if deletedAt == nil {
		t.Error("mem-absorbed should be soft-deleted after merge")
	}
}

func TestApplyAction_Merge_NoRelatedID_Error(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-solo", "content")
	insertTestDreamAction(t, ctx, db, "act-no-rel", "mem-solo", "", "merge", 0.9, "proposed")

	_, err := c.ApplyAction(ctx, "act-no-rel")
	if err == nil {
		t.Fatal("expected error for merge without related_memory_id")
	}
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestClustersFromPairs_UnionFind(t *testing.T) {
	pairs := [][2]string{{"a", "b"}, {"b", "c"}, {"x", "y"}, {"c", "d"}}
	clusters := clustersFromPairs(pairs, 3)
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster >=3 (a-b-c-d), got %d: %v", len(clusters), clusters)
	}
	if len(clusters[0]) != 4 || clusters[0][0] != "a" {
		t.Errorf("cluster should be the sorted a-d component, got %v", clusters[0])
	}
	if got := clustersFromPairs([][2]string{{"p", "q"}}, 3); len(got) != 0 {
		t.Errorf("pairs below minSize must not cluster, got %v", got)
	}
}

func TestApplyAction_Synthesize_CreatesSummaryAndDemotes(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-a", "the deploy pipeline requires TAG_NAME and DEPLOY_REASON")
	insertTestMemory(t, ctx, db, "mem-b", "deploy pipeline validation needs TAG_NAME plus a reason")
	insertTestMemory(t, ctx, db, "mem-c", "pipeline deploys are tag-driven with mandatory reason")
	insertTestDreamAction(t, ctx, db, "act-syn", "mem-a", "mem-b,mem-c", "synthesize", 0.9, "proposed")

	result, err := c.ApplyAction(ctx, "act-syn")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" {
		t.Fatalf("want applied, got %q (%s)", result.Status, result.Message)
	}

	// A new summary memory exists, carrying the member IDs.
	var newID, content, meta string
	if err := db.QueryRowContext(ctx, `
		SELECT id, content, metadata FROM memories
		WHERE source = 'dream_consolidation' AND deleted_at IS NULL`).Scan(&newID, &content, &meta); err != nil {
		t.Fatalf("synthesis memory not found: %v", err)
	}
	if !strings.Contains(content, "Consolidated from 3") {
		t.Errorf("synthesis content unexpected: %s", content)
	}
	for _, id := range []string{"mem-a", "mem-b", "mem-c"} {
		if !strings.Contains(meta, id) {
			t.Errorf("metadata.consolidated missing %s: %s", id, meta)
		}
	}

	// Members are demoted — never deleted.
	for _, id := range []string{"mem-a", "mem-b", "mem-c"} {
		var status, rule, into string
		var deletedAt *string
		if err := db.QueryRowContext(ctx, `
			SELECT COALESCE(json_extract(metadata,'$.curation_status'),''),
			       COALESCE(json_extract(metadata,'$.curation_rule'),''),
			       COALESCE(json_extract(metadata,'$.consolidated_into'),''),
			       deleted_at
			FROM memories WHERE id = ?`, id).Scan(&status, &rule, &into, &deletedAt); err != nil {
			t.Fatal(err)
		}
		if status != "low_signal" || rule != "consolidated" || into != newID {
			t.Errorf("%s: status=%q rule=%q into=%q (want low_signal/consolidated/%s)", id, status, rule, into, newID)
		}
		if deletedAt != nil {
			t.Errorf("%s must be demoted, NOT deleted", id)
		}
	}
}

// TestApplyAction_Synthesize_RecordsBaseRevision covers the synthesis row's
// membership in the temporal ledger: curation resolves a memory through
// memories.current_revision_id and the embedding queue joins on it, so a
// synthesis without a base revision would be scored by neither.
func TestApplyAction_Synthesize_RecordsBaseRevision(t *testing.T) {
	ctx := context.Background()
	db := setupConsolidatorTestDB(t)
	c := NewConsolidator(db, nil)

	insertTestMemory(t, ctx, db, "mem-a", "the deploy pipeline requires TAG_NAME and DEPLOY_REASON")
	insertTestMemory(t, ctx, db, "mem-b", "deploy pipeline validation needs TAG_NAME plus a reason")
	insertTestMemory(t, ctx, db, "mem-c", "pipeline deploys are tag-driven with mandatory reason")
	insertTestDreamAction(t, ctx, db, "act-rev", "mem-a", "mem-b,mem-c", "synthesize", 0.9, "proposed")

	result, err := c.ApplyAction(ctx, "act-rev")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" {
		t.Fatalf("want applied, got %q (%s)", result.Status, result.Message)
	}

	var id, logicalID, revisionID string
	if err := db.QueryRowContext(ctx, `
		SELECT id, COALESCE(logical_id, ''), COALESCE(current_revision_id, '')
		FROM memories WHERE source = 'dream_consolidation' AND deleted_at IS NULL`,
	).Scan(&id, &logicalID, &revisionID); err != nil {
		t.Fatalf("synthesis memory not found: %v", err)
	}
	if logicalID != id {
		t.Errorf("logical_id = %q, want %q", logicalID, id)
	}
	if revisionID == "" {
		t.Fatal("current_revision_id is empty: the synthesis is orphaned from the ledger")
	}

	var mode, category string
	var tombstone bool
	var validTo, systemTo sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT temporal_mode, category, is_tombstone, valid_to, system_to
		FROM memory_revisions WHERE revision_id = ? AND memory_id = ? AND logical_id = ?`,
		revisionID, id, id,
	).Scan(&mode, &category, &tombstone, &validTo, &systemTo); err != nil {
		t.Fatalf("query revision: %v", err)
	}
	if mode != "supersede" {
		t.Errorf("temporal_mode = %q, want supersede", mode)
	}
	if category != "summary" {
		t.Errorf("revision category = %q, want summary", category)
	}
	if tombstone {
		t.Error("base revision must not be a tombstone")
	}
	if validTo.Valid || systemTo.Valid {
		t.Error("base revision must stay open on both time axes")
	}
}
