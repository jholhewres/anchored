package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A row deleted by a raw UPDATE outside the ledger (dream before v0.20, purge,
// curation clean) still has an active current revision. Undoing that delete
// makes the current view agree with the ledger again, without inventing a
// revision the ledger never needed.

func newUntrackedDeleteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "undo.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestUndoUntrackedDelete_RestoresRawDeletedRow(t *testing.T) {
	ctx := context.Background()
	store := newUntrackedDeleteStore(t)
	if _, err := store.SaveTemporal(ctx, Memory{ID: "m1", Category: "fact", Content: "kept fact"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'm1'"); err != nil {
		t.Fatal(err)
	}
	before := countRevisions(t, store, "m1")

	changed, err := store.UndoUntrackedDelete(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the raw-deleted row to be restored")
	}
	if got, err := store.Get(ctx, "m1"); err != nil || got == nil {
		t.Fatalf("restored memory must be readable again, got %v, %v", got, err)
	}
	if after := countRevisions(t, store, "m1"); after != before {
		t.Errorf("undoing an untracked delete must not write a revision: %d -> %d", before, after)
	}
}

func TestUndoUntrackedDelete_LeavesLedgerTombstonesAlone(t *testing.T) {
	ctx := context.Background()
	store := newUntrackedDeleteStore(t)
	if _, err := store.SaveTemporal(ctx, Memory{ID: "m1", Category: "fact", Content: "forgotten on purpose"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}

	changed, err := store.UndoUntrackedDelete(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a delete recorded in the ledger is undone with Restore, not by clearing deleted_at")
	}
	if got, _ := store.Get(ctx, "m1"); got != nil {
		t.Error("the tombstoned memory must stay deleted")
	}
}

func TestUndoUntrackedDelete_NoopOnLiveOrMissing(t *testing.T) {
	ctx := context.Background()
	store := newUntrackedDeleteStore(t)
	if _, err := store.SaveTemporal(ctx, Memory{ID: "live", Category: "fact", Content: "still here"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"live", "missing"} {
		changed, err := store.UndoUntrackedDelete(ctx, id)
		if err != nil || changed {
			t.Errorf("%s: expected no change, got changed=%v err=%v", id, changed, err)
		}
	}
}

func TestServiceRestoreDeleted_HandlesBothKindsOfDelete(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)
	for _, id := range []string{"raw", "ledger"} {
		if _, err := store.SaveTemporal(ctx, Memory{ID: id, Category: "fact", Content: "content " + id}, TemporalWriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'raw'"); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(ctx, "ledger"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"raw", "ledger"} {
		changed, err := svc.RestoreDeleted(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if !changed {
			t.Errorf("%s: expected a restore", id)
		}
		if got, _ := store.Get(ctx, id); got == nil {
			t.Errorf("%s: memory must be live after restore", id)
		}
	}

	changed, err := svc.RestoreDeleted(ctx, "raw")
	if err != nil || changed {
		t.Errorf("restoring a live memory is a no-op, got changed=%v err=%v", changed, err)
	}
}

// A process that loaded its cache after the raw delete does not hold the
// vector; undoing the delete must put it back so vector search finds the
// memory without a restart.
func TestUndoUntrackedDelete_RefreshesVectorCache(t *testing.T) {
	ctx := context.Background()
	store := newUntrackedDeleteStore(t)
	if err := store.Save(ctx, Memory{ID: "m1", Content: "vector backed", ContentHash: contentHash("vector backed"), Category: "fact", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{})
	if err != nil || current == nil {
		t.Fatalf("current=%v err=%v", current, err)
	}
	identity := EmbeddingIdentity{Provider: "test", Model: "a", ModelRevision: "1", Dimensions: 2, Normalization: "l2"}
	gen, err := store.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: current.RevisionID, MemoryID: "m1", GenerationID: gen.ID,
		SemanticSpaceID: gen.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identity, ContentHash: current.Memory.ContentHash, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'm1'"); err != nil {
		t.Fatal(err)
	}
	store.VectorCache().Remove("m1")

	if changed, err := store.UndoUntrackedDelete(ctx, "m1"); err != nil || !changed {
		t.Fatalf("undo: changed=%v err=%v", changed, err)
	}
	if got, ok := store.VectorCache().Get("m1"); !ok || got[0] != 1 {
		t.Errorf("restored memory must be back in the vector cache, got %v ok=%v", got, ok)
	}
}

// A row whose current revision is missing (legacy ids adopted by migration
// 021) cannot be restored by either path; reporting it as restored would
// hide that it is still deleted.
func TestServiceRestoreDeleted_ReportsOnlyRealRestores(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)
	if _, err := store.DB().Exec(`INSERT INTO memories (id, category, content, content_hash, logical_id, current_revision_id, deleted_at)
		VALUES ('orphan', 'fact', 'no revision behind me', 'h', 'orphan', 'legacy:orphan:tombstone', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	changed, _ := svc.RestoreDeleted(ctx, "orphan")
	var stillDeleted bool
	if err := store.DB().QueryRow("SELECT deleted_at IS NOT NULL FROM memories WHERE id = 'orphan'").Scan(&stillDeleted); err != nil {
		t.Fatal(err)
	}
	if changed && stillDeleted {
		t.Error("RestoreDeleted reported a restore for a memory that is still deleted")
	}
}

func TestServiceSoftDeleteIfActive_NotifiesOnlyOnRealDeletes(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)
	obs := &mockObserver{}
	svc.RegisterObserver(obs)
	if _, err := store.SaveTemporal(ctx, Memory{ID: "m1", Category: "fact", Content: "delete me once"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.SoftDeleteIfActive(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
	}
	// Observers run in their own goroutines: wait for the first call, then
	// give a second one the chance to show up before asserting it did not.
	calls := func() []deletedCall {
		obs.mu.Lock()
		defer obs.mu.Unlock()
		return append([]deletedCall(nil), obs.deletedCalls...)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls(); len(got) != 1 || got[0].ID != "m1" {
		t.Errorf("expected exactly one delete notification, got %+v", got)
	}
}
