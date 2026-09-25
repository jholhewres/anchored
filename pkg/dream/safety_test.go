package dream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// These tests pin the rules that keep dream from deleting what it should not:
// a duplicate is only a duplicate inside one project, the oldest copy is the
// one kept, deletes go through the temporal ledger so they can be restored,
// and a stale proposal whose keeper is gone or lives elsewhere is refused.

func insertAnalyzerMemory(t *testing.T, db *sql.DB, id, content, hash string, projectID any, createdAt time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		"INSERT INTO memories (id, project_id, content, category, content_hash, created_at, logical_id, current_revision_id) VALUES (?, ?, ?, 'fact', ?, ?, ?1, ?1)",
		id, projectID, content, hash, createdAt)
	if err != nil {
		t.Fatal(err)
	}
}

func dedupActions(report *DreamReport) []DreamAction {
	var out []DreamAction
	for _, a := range report.Actions {
		if a.ActionType == "dedup" {
			out = append(out, a)
		}
	}
	return out
}

func TestAnalyze_ExactDedup_KeepsOldestWithinProject(t *testing.T) {
	db := setupTestDB(t)
	t0 := time.Now().Add(-time.Hour)
	insertAnalyzerMemory(t, db, "mem-old", "same text", "h1", "proj-1", t0)
	insertAnalyzerMemory(t, db, "mem-new", "same text", "h1", "proj-1", t0.Add(time.Minute))

	report, err := NewAnalyzer(db, nil, DefaultDreamConfig(), nil).Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	actions := dedupActions(report)
	if len(actions) != 1 {
		t.Fatalf("expected 1 dedup action, got %d: %+v", len(actions), actions)
	}
	if actions[0].MemoryID != "mem-new" || actions[0].RelatedMemoryID != "mem-old" {
		t.Errorf("dedup must delete the newer copy and keep the oldest, got delete=%q keep=%q",
			actions[0].MemoryID, actions[0].RelatedMemoryID)
	}
}

func TestAnalyze_ExactDedup_IgnoresOtherProjects(t *testing.T) {
	db := setupTestDB(t)
	t0 := time.Now().Add(-time.Hour)
	insertAnalyzerMemory(t, db, "mem-p1", "same text", "h1", "proj-1", t0)
	insertAnalyzerMemory(t, db, "mem-p2", "same text", "h1", "proj-2", t0.Add(time.Minute))
	insertAnalyzerMemory(t, db, "mem-global", "same text", "h1", nil, t0.Add(2*time.Minute))

	report, err := NewAnalyzer(db, nil, DefaultDreamConfig(), nil).Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if actions := dedupActions(report); len(actions) != 0 {
		t.Errorf("identical text in different projects is not a duplicate, got %d actions: %+v", len(actions), actions)
	}
	if report.ExactDupes != 0 {
		t.Errorf("expected 0 exact dupes across projects, got %d", report.ExactDupes)
	}
}

func TestAnalyze_SemanticTiersOffByDefault(t *testing.T) {
	db := setupTestDB(t)
	cache := memory.NewVectorCache(nil)
	for i, id := range []string{"mem-a", "mem-b", "mem-c"} {
		insertAnalyzerMemory(t, db, id, "text "+id, "h-"+id, "proj-1", time.Now().Add(time.Duration(i)*time.Minute))
		cache.Put(id, []float32{1.0, 0.5, 0.3, 0.2})
	}

	report, err := NewAnalyzer(db, cache, DefaultDreamConfig(), nil).Analyze(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if report.NearDupes != 0 {
		t.Errorf("near-dup tier must be off by default, got %d near-dupes", report.NearDupes)
	}
	for _, a := range report.Actions {
		if a.ActionType == "synthesize" || a.ActionType == "contradiction" {
			t.Errorf("semantic tier produced %q with SemanticTiers off: %+v", a.ActionType, a)
		}
	}
}

func newLedgerStore(t *testing.T) *memory.SQLiteStore {
	t.Helper()
	store, err := memory.NewSQLiteStore(t.TempDir()+"/dream.db", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func saveLedgerMemory(t *testing.T, store *memory.SQLiteStore, id, content string, projectID *string) {
	t.Helper()
	_, err := store.SaveTemporal(context.Background(), memory.Memory{
		ID: id, Category: "fact", Content: content, ProjectID: projectID,
	}, memory.TemporalWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func isDeleted(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var deletedAt sql.NullString
	if err := db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", id).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	return deletedAt.Valid
}

func strPtr(s string) *string { return &s }

func TestConsolidate_DeletesThroughLedgerAndIsRestorable(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	db := store.DB()
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-1"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))

	c := NewConsolidator(db, store, nil)
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "a1", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 1 {
		t.Fatalf("expected 1 soft delete, got %+v", result)
	}

	var tombstones int
	if err := db.QueryRow("SELECT COUNT(*) FROM memory_revisions WHERE memory_id = 'dup' AND is_tombstone").Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 {
		t.Errorf("a dream delete must leave a tombstone revision, got %d", tombstones)
	}

	if err := store.Restore(ctx, "dup"); err != nil {
		t.Fatal(err)
	}
	if isDeleted(t, db, "dup") {
		t.Error("a dream delete must be restorable through the ledger")
	}
}

func TestConsolidate_CountsOnlyRealDeletions(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-1"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))

	c := NewConsolidator(store.DB(), store, nil)
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "a1", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
		{ID: "a2", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
		{ID: "a3", MemoryID: "missing", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 1 || result.Skipped != 2 {
		t.Errorf("only the one row that changed counts, got %+v", result)
	}
}

func TestConsolidate_RefusesCrossProjectKeeper(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-2"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))

	c := NewConsolidator(store.DB(), store, nil)
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "a1", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 0 || isDeleted(t, store.DB(), "dup") {
		t.Errorf("a keeper in another project must not justify a delete, got %+v", result)
	}
}

func TestConsolidate_RefusesWhenKeeperIsGone(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-1"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))
	if err := store.SoftDelete(ctx, "keeper"); err != nil {
		t.Fatal(err)
	}

	c := NewConsolidator(store.DB(), store, nil)
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "a1", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 0 || isDeleted(t, store.DB(), "dup") {
		t.Errorf("deleting the last live copy loses the memory, got %+v", result)
	}
}

func TestApplyAction_Dedup_RefusesCrossProject(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	db := store.DB()
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-2"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))
	insertTestDreamAction(t, ctx, db, "act-x", "dup", "keeper", "dedup", 1.0, "proposed")

	c := NewConsolidator(db, store, nil)
	if _, err := c.ApplyAction(ctx, "act-x"); err == nil {
		t.Fatal("applying a cross-project dedup must fail")
	}
	if isDeleted(t, db, "dup") {
		t.Error("the refused action must leave the memory alive")
	}
	var status string
	if err := db.QueryRow("SELECT status FROM dream_actions WHERE id = 'act-x'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "proposed" {
		t.Errorf("a refused action stays proposed, got %q", status)
	}
}

// Stored proposals outlive the text they were computed on (an edit since the
// analysis), and 397k near-duplicate proposals came from a collapsed vector
// space: a dedup only deletes a memory whose content is identical to its
// keeper's.
func TestDedup_RefusesWhenContentsDiffer(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	db := store.DB()
	saveLedgerMemory(t, store, "keeper", "the deploy needs TAG_NAME", strPtr("proj-1"))
	saveLedgerMemory(t, store, "near", "deploys are tag driven, with a mandatory reason", strPtr("proj-1"))
	insertTestDreamAction(t, ctx, db, "act-near", "near", "keeper", "dedup", 0.91, "proposed")

	c := NewConsolidator(db, store, nil)
	if _, err := c.ApplyAction(ctx, "act-near"); err == nil {
		t.Error("applying a dedup between different texts must fail")
	}
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "act-near", MemoryID: "near", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 0.99},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 0 || isDeleted(t, db, "near") {
		t.Errorf("a non-identical memory must survive dedup, got %+v", result)
	}
}

func TestApplyAction_Merge_RefusesCrossProjectOrGoneKeeper(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(t *testing.T, store *memory.SQLiteStore){
		"cross-project": func(t *testing.T, store *memory.SQLiteStore) {
			saveLedgerMemory(t, store, "keeper", "combined text", strPtr("proj-2"))
			saveLedgerMemory(t, store, "absorbed", "absorbed text", strPtr("proj-1"))
		},
		"keeper-gone": func(t *testing.T, store *memory.SQLiteStore) {
			saveLedgerMemory(t, store, "keeper", "combined text", strPtr("proj-1"))
			saveLedgerMemory(t, store, "absorbed", "absorbed text", strPtr("proj-1"))
			if err := store.SoftDelete(ctx, "keeper"); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newLedgerStore(t)
			db := store.DB()
			setup(t, store)
			insertTestDreamAction(t, ctx, db, "act-merge", "keeper", "absorbed", "merge", 0.95, "proposed")

			if _, err := NewConsolidator(db, store, nil).ApplyAction(ctx, "act-merge"); err == nil {
				t.Fatal("the merge must be refused")
			}
			if isDeleted(t, db, "absorbed") {
				t.Error("the absorbed memory must survive a refused merge")
			}
			var meta sql.NullString
			if err := db.QueryRow("SELECT metadata FROM memories WHERE id = 'keeper'").Scan(&meta); err != nil {
				t.Fatal(err)
			}
			if containsSubstring(meta.String, "absorbed") {
				t.Errorf("a refused merge must not leave partial metadata on the keeper: %q", meta.String)
			}
			var status string
			if err := db.QueryRow("SELECT status FROM dream_actions WHERE id = 'act-merge'").Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "proposed" {
				t.Errorf("a refused merge stays proposed, got %q", status)
			}
		})
	}
}

func TestDedup_RefusesAMemoryAsItsOwnKeeper(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	db := store.DB()
	saveLedgerMemory(t, store, "only", "the only copy", strPtr("proj-1"))
	insertTestDreamAction(t, ctx, db, "act-self", "only", "only", "dedup", 1.0, "proposed")

	c := NewConsolidator(db, store, nil)
	if _, err := c.ApplyAction(ctx, "act-self"); err == nil {
		t.Error("a memory cannot be deduplicated against itself")
	}
	result, err := c.Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "act-self", MemoryID: "only", RelatedMemoryID: "only", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.SoftDeleted != 0 || isDeleted(t, db, "only") {
		t.Errorf("the last copy must survive, got %+v", result)
	}
}

func TestConsolidate_MarksAppliedProposals(t *testing.T) {
	ctx := context.Background()
	store := newLedgerStore(t)
	db := store.DB()
	saveLedgerMemory(t, store, "keeper", "same text", strPtr("proj-1"))
	saveLedgerMemory(t, store, "dup", "same text", strPtr("proj-1"))
	insertTestDreamAction(t, ctx, db, "act-1", "dup", "keeper", "dedup", 1.0, "proposed")

	if _, err := NewConsolidator(db, store, nil).Consolidate(ctx, &DreamReport{Actions: []DreamAction{
		{ID: "act-1", MemoryID: "dup", RelatedMemoryID: "keeper", ActionType: "dedup", Confidence: 1.0},
	}}, DefaultDreamConfig()); err != nil {
		t.Fatal(err)
	}
	var status string
	var appliedAt sql.NullString
	if err := db.QueryRow("SELECT status, applied_at FROM dream_actions WHERE id = 'act-1'").Scan(&status, &appliedAt); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || !appliedAt.Valid {
		t.Errorf("an applied proposal must be marked, got status=%q applied_at=%v", status, appliedAt)
	}
}
