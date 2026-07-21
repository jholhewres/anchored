package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestMemoryRevisionMigrationFreshAndIdempotent(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := Migrate(store.DB()); err != nil {
			t.Fatalf("Migrate pass %d: %v", i+1, err)
		}
	}
	var migrations, queueMigrations, revisions int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE name = '018_memory_revision_ledger'`,
	).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM migrations WHERE name = '019_processing_jobs_remote_outbox'`,
	).Scan(&queueMigrations); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_revisions`).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if migrations != 1 || queueMigrations != 1 || revisions != 0 {
		t.Fatalf("fresh migrations = (%d ledger, %d queue, %d revisions), want (1, 1, 0)",
			migrations, queueMigrations, revisions)
	}
}

func TestMemoryRevisionMigrationBackfillsLegacyRowsDeterministically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(legacyTemporalFixtureSchema); err != nil {
		t.Fatalf("create legacy fixture: %v", err)
	}
	// Migrate checks exact names; mark all intervening migrations as already
	// applied because this fixture represents the schema immediately before 018.
	for _, name := range preTemporalMigrationNames {
		if _, err := db.Exec(`INSERT OR IGNORE INTO migrations(name) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}

	created := "2026-01-01 00:00:00"
	deleted := "2026-01-02 00:00:00"
	if _, err := db.Exec(`INSERT INTO memories
		(id, category, content, content_hash, source, created_at, updated_at)
		VALUES ('active', 'fact', 'active content', 'ha', 'test', ?, ?)`, created, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO memories
		(id, category, content, content_hash, source, created_at, updated_at, deleted_at)
		VALUES ('deleted', 'fact', 'deleted content', 'hd', 'test', ?, ?, ?)`, created, deleted, deleted); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate pass %d: %v", i+1, err)
		}
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_revisions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("revision count = %d, want 4", count)
	}
	assertCurrentRevision(t, db, "active", "active", "legacy:active:base")
	assertCurrentRevision(t, db, "deleted", "deleted", "legacy:deleted:tombstone")

	var tombstone bool
	if err := db.QueryRow(`SELECT is_tombstone FROM memory_revisions
		WHERE revision_id = 'legacy:deleted:tombstone'`).Scan(&tombstone); err != nil {
		t.Fatal(err)
	}
	if !tombstone {
		t.Fatal("deleted legacy row was not backfilled as a tombstone")
	}

	legacyStore := &SQLiteStore{db: db}
	validBeforeDelete := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	knownAfterDelete := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	historical, err := legacyStore.RevisionAt(context.Background(), "deleted", TemporalQueryOptions{
		ValidAt: &validBeforeDelete,
		KnownAt: &knownAfterDelete,
	})
	if err != nil || historical == nil || historical.Memory.Content != "deleted content" {
		t.Fatalf("legacy historical lookup = (%#v, %v)", historical, err)
	}
}

func TestDefaultSaveSupersedesAtTransactionStartAndPreservesKnownHistory(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)

	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "before", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(t2)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "after", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	current, err := store.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Content != "after" {
		t.Fatalf("current = %#v, want after", current)
	}

	assertRevisionContent(t, store, "m1", t2.Add(time.Second), t1.Add(time.Second), "before", false)
	assertRevisionContent(t, store, "m1", t1.Add(time.Second), t2.Add(time.Second), "before", false)
	assertRevisionContent(t, store, "m1", t2, t2.Add(time.Second), "after", false)
	assertNoCurrentOverlap(t, store, "m1")
}

func TestCorrectionAdvancesSystemTimeWithoutChangingValidTime(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "typo", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	store.now = clockAt(t2)
	revision, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "corrected", Source: "test"},
		TemporalWriteOptions{Mode: TemporalCorrect, LogicalID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if !revision.ValidFrom.Equal(t1) || revision.ValidTo != nil {
		t.Fatalf("corrected valid interval = [%s, %v), want [%s, infinity)", revision.ValidFrom, revision.ValidTo, t1)
	}

	assertRevisionContent(t, store, "m1", t1.Add(time.Second), t1.Add(time.Second), "typo", false)
	assertRevisionContent(t, store, "m1", t1.Add(time.Second), t2.Add(time.Second), "corrected", false)
	assertNoCurrentOverlap(t, store, "m1")
}

func TestCorrectionRejectsValidTimeChanges(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "original", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	changedStart := t1.Add(time.Second)
	store.now = clockAt(t2)
	if _, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "invalid correction", Source: "test"},
		TemporalWriteOptions{
			Mode:      TemporalCorrect,
			LogicalID: "m1",
			ValidFrom: &changedStart,
		},
	); !errors.Is(err, ErrCorrectionIntervalChange) {
		t.Fatalf("changed correction start error = %v", err)
	}
	current, err := store.Get(ctx, "m1")
	if err != nil || current == nil || current.Content != "original" {
		t.Fatalf("current after rejected correction = (%#v, %v)", current, err)
	}

	finiteEnd := instant(3)
	if _, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "invalid correction", Source: "test"},
		TemporalWriteOptions{
			Mode:      TemporalCorrect,
			LogicalID: "m1",
			ValidTo:   &finiteEnd,
		},
	); !errors.Is(err, ErrCorrectionIntervalChange) {
		t.Fatalf("changed correction end error = %v", err)
	}
}

func TestSoftDeleteAndRestoreAreHistoricalOperations(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	t3 := instant(3)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "alive", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	store.now = clockAt(t2)
	if err := store.SoftDelete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if current, err := store.Get(ctx, "m1"); err != nil || current != nil {
		t.Fatalf("Get after tombstone = (%#v, %v), want nil, nil", current, err)
	}
	assertRevisionContent(t, store, "m1", t2, t2.Add(time.Second), "alive", true)
	assertRevisionContent(t, store, "m1", t1.Add(time.Second), t2.Add(time.Second), "alive", false)

	store.now = clockAt(t3)
	if err := store.Restore(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(ctx, "m1")
	if err != nil || current == nil || current.Content != "alive" {
		t.Fatalf("Get after restore = (%#v, %v)", current, err)
	}
	assertRevisionContent(t, store, "m1", t2.Add(time.Second), t3.Add(-time.Second), "alive", true)
	assertRevisionContent(t, store, "m1", t3, t3.Add(time.Second), "alive", false)
	assertNoCurrentOverlap(t, store, "m1")
}

func TestTemporalSelectionDefaultsAndHalfOpenBoundaries(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	t3 := instant(3)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "v1", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(t2)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "v2", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	// End is exclusive and start is inclusive.
	assertRevisionContent(t, store, "m1", t2.Add(-time.Nanosecond), t3, "v1", false)
	assertRevisionContent(t, store, "m1", t2, t3, "v2", false)

	// known_at only defaults valid_at to the same instant.
	knownOnly, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{KnownAt: &t1})
	if err != nil || knownOnly == nil || knownOnly.Memory.Content != "v1" {
		t.Fatalf("known-only = (%#v, %v)", knownOnly, err)
	}

	// valid_at only captures known_at once from the query-start clock.
	store.now = clockAt(t3)
	validOnly, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{ValidAt: &t2})
	if err != nil || validOnly == nil || validOnly.Memory.Content != "v2" {
		t.Fatalf("valid-only = (%#v, %v)", validOnly, err)
	}

	// No axes resolves exactly the compatibility current-view pointer.
	current, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{})
	if err != nil || current == nil || current.Memory.Content != "v2" {
		t.Fatalf("current revision = (%#v, %v)", current, err)
	}
}

func TestRetroactiveSupersedeSplitsWithoutCurrentOverlap(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	t3 := instant(3)
	t4 := instant(4)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "baseline", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	store.now = clockAt(t4)
	if _, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "temporary", Source: "test"},
		TemporalWriteOptions{
			Mode:      TemporalSupersede,
			LogicalID: "m1",
			ValidFrom: &t2,
			ValidTo:   &t3,
		}); err != nil {
		t.Fatal(err)
	}

	assertRevisionContent(t, store, "m1", t1.Add(time.Second), t4.Add(time.Second), "baseline", false)
	assertRevisionContent(t, store, "m1", t2, t4.Add(time.Second), "temporary", false)
	assertRevisionContent(t, store, "m1", t3, t4.Add(time.Second), "baseline", false)
	assertNoCurrentOverlap(t, store, "m1")
	current, err := store.Get(ctx, "m1")
	if err != nil || current == nil || current.Content != "baseline" {
		t.Fatalf("compatibility current view = (%#v, %v), want baseline", current, err)
	}
	pointed, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{})
	if err != nil || pointed == nil || pointed.Memory.Content != "baseline" {
		t.Fatalf("current revision pointer = (%#v, %v), want baseline", pointed, err)
	}
}

func TestTemporalSaveRejectsInvalidIntervalAndRollsBackCurrentView(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "stable", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "invalid", Source: "test"},
		TemporalWriteOptions{LogicalID: "m1", ValidFrom: &t2, ValidTo: &t2},
	); !errors.Is(err, ErrInvalidTemporalInterval) {
		t.Fatalf("invalid interval error = %v", err)
	}

	if _, err := store.DB().Exec(`CREATE TRIGGER reject_failed_revision
		BEFORE INSERT ON memory_revisions
		WHEN NEW.content = 'fail'
		BEGIN SELECT RAISE(ABORT, 'injected revision failure'); END`); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(t2)
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "fail", Source: "test"}); err == nil {
		t.Fatal("Save succeeded despite injected revision failure")
	}

	current, err := store.Get(ctx, "m1")
	if err != nil || current == nil || current.Content != "stable" {
		t.Fatalf("current after rollback = (%#v, %v), want stable", current, err)
	}
	var open int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM memory_revisions
		WHERE logical_id = 'm1' AND system_to IS NULL`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("open revisions after rollback = %d, want 1", open)
	}
}

func TestTemporalSaveRejectsFutureProjectionBoundaries(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	t3 := instant(3)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{
		ID: "m1", Category: "fact", Content: "stable", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}

	store.now = clockAt(t2)
	for name, opts := range map[string]TemporalWriteOptions{
		"future start": {
			LogicalID: "m1",
			ValidFrom: &t3,
		},
		"future end": {
			LogicalID: "m1",
			ValidFrom: &t1,
			ValidTo:   &t3,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.SaveTemporal(ctx, Memory{
				ID: "m1", Category: "fact", Content: "scheduled", Source: "test",
			}, opts); !errors.Is(err, ErrFutureTemporalInterval) {
				t.Fatalf("future boundary error = %v", err)
			}
		})
	}

	current, err := store.Get(ctx, "m1")
	if err != nil || current == nil || current.Content != "stable" {
		t.Fatalf("current after rejected future write = (%#v, %v)", current, err)
	}
	if got := revisionCount(t, store, "m1"); got != 1 {
		t.Fatalf("revision count after rejected future writes = %d, want 1", got)
	}
}

func TestTemporalLogicalIdentityCannotAcquireSecondPhysicalMemory(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)

	first, err := store.SaveTemporal(ctx, Memory{
		ID: "physical-1", Category: "fact", Content: "first", Source: "test",
	}, TemporalWriteOptions{LogicalID: "logical"})
	if err != nil {
		t.Fatal(err)
	}
	if first.MemoryID != "physical-1" || first.LogicalID != "logical" {
		t.Fatalf("first revision identity = (%q, %q)", first.MemoryID, first.LogicalID)
	}

	store.now = clockAt(t2)
	if _, err := store.SaveTemporal(ctx, Memory{
		ID: "physical-2", Category: "fact", Content: "second", Source: "test",
	}, TemporalWriteOptions{LogicalID: "logical"}); !errors.Is(err, ErrTemporalIdentityConflict) {
		t.Fatalf("second physical identity error = %v", err)
	}

	resolved, err := store.SaveTemporal(ctx, Memory{
		Category: "fact", Content: "updated through logical identity", Source: "test",
	}, TemporalWriteOptions{LogicalID: "logical"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.MemoryID != "physical-1" {
		t.Fatalf("logical identity resolved memory %q, want physical-1", resolved.MemoryID)
	}
	var rows int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE logical_id = 'logical'`,
	).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("physical rows for logical identity = %d, want 1", rows)
	}
}

func TestHardDeletePurgesRevisionLedger(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	store.now = clockAt(instant(1))
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "purge", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = 'm1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("hard delete left %d revisions", count)
	}
}

func TestUpdateMetadataCreatesCorrectionAndRollsBackAtomically(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	t1 := instant(1)
	t2 := instant(2)
	store.now = clockAt(t1)
	if err := store.Save(ctx, Memory{
		ID: "m1", Category: "fact", Content: "stable", Source: "test",
		Metadata: map[string]any{"state": "old"},
	}); err != nil {
		t.Fatal(err)
	}
	beforeUpdate, err := store.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`
		CREATE TABLE metadata_update_probe (id INTEGER);
		CREATE TRIGGER metadata_update_reindexed
		AFTER UPDATE OF content, keywords ON memories
		BEGIN INSERT INTO metadata_update_probe(id) VALUES (1); END;
	`); err != nil {
		t.Fatal(err)
	}

	store.now = clockAt(t2)
	if err := store.UpdateMetadata(ctx, "m1", map[string]any{"state": "new"}); err != nil {
		t.Fatal(err)
	}
	currentAfterUpdate, err := store.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !currentAfterUpdate.UpdatedAt.Equal(beforeUpdate.UpdatedAt) {
		t.Fatalf("metadata update changed updated_at: got %s want %s",
			currentAfterUpdate.UpdatedAt, beforeUpdate.UpdatedAt)
	}
	var reindexes int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM metadata_update_probe`).Scan(&reindexes); err != nil {
		t.Fatal(err)
	}
	if reindexes != 0 {
		t.Fatalf("metadata update triggered %d content/keywords updates", reindexes)
	}
	before, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{ValidAt: ptrTime(t1), KnownAt: ptrTime(t1)})
	if err != nil || metadataState(before) != "old" {
		t.Fatalf("metadata before correction = (%q, %v)", metadataState(before), err)
	}
	after, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{ValidAt: ptrTime(t1), KnownAt: ptrTime(t2)})
	if err != nil || metadataState(after) != "new" {
		t.Fatalf("metadata after correction = (%q, %v)", metadataState(after), err)
	}

	if _, err := store.DB().Exec(`CREATE TRIGGER reject_bad_metadata
		BEFORE INSERT ON memory_revisions
		WHEN NEW.metadata LIKE '%bad%'
		BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetadata(ctx, "m1", map[string]any{"state": "bad"}); err == nil {
		t.Fatal("UpdateMetadata succeeded despite injected revision failure")
	}
	current, err := store.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if state := current.Metadata.(map[string]any)["state"]; state != "new" {
		t.Fatalf("current metadata after rollback = %v, want new", state)
	}
}

func TestConcurrentPartialUpdatesDoNotRestoreStaleFields(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	store.now = clockAt(instant(1))
	if err := store.Save(ctx, Memory{
		ID: "m1", Category: "fact", Content: "old", Source: "test",
		Metadata: map[string]any{"state": "old"},
	}); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(instant(2))

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs <- store.Update(ctx, "m1", "new content", "decision")
	}()
	go func() {
		defer wg.Done()
		<-start
		errs <- store.UpdateMetadata(ctx, "m1", map[string]any{"state": "new"})
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	current, err := store.Get(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Content != "new content" || current.Category != "decision" {
		t.Fatalf("content/category = (%q, %q)", current.Content, current.Category)
	}
	if state := current.Metadata.(map[string]any)["state"]; state != "new" {
		t.Fatalf("metadata state = %v, want new", state)
	}
	assertNoCurrentOverlap(t, store, "m1")
}

func TestSoftDeleteByScopeRollsBackAllTombstonesOnFailure(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	projectID := "project"
	store.now = clockAt(instant(1))
	for _, id := range []string{"m1", "m2"} {
		if err := store.Save(ctx, Memory{
			ID: id, ProjectID: &projectID, Category: "fact",
			Content: "scoped " + id, Source: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`
		CREATE TRIGGER reject_second_scope_tombstone
		BEFORE INSERT ON memory_revisions
		WHEN NEW.memory_id = 'm2' AND NEW.is_tombstone = TRUE
		BEGIN SELECT RAISE(ABORT, 'injected scope failure'); END;
	`); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(instant(2))
	if _, err := store.DeleteByScope(ctx, DeleteScopeOptions{
		ProjectID: projectID,
	}); err == nil {
		t.Fatal("DeleteByScope succeeded despite injected second-row failure")
	}
	for _, id := range []string{"m1", "m2"} {
		current, err := store.Get(ctx, id)
		if err != nil || current == nil {
			t.Fatalf("%s was partially deleted: memory=%#v err=%v", id, current, err)
		}
		revision, err := store.RevisionAt(ctx, id, TemporalQueryOptions{})
		if err != nil || revision == nil || revision.IsTombstone {
			t.Fatalf("%s current revision changed after rollback: %#v err=%v", id, revision, err)
		}
	}
}

func TestTombstoneMutationPreconditionsPreserveLegacyNoOps(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	store.now = clockAt(instant(1))
	if err := store.Save(ctx, Memory{ID: "m1", Category: "fact", Content: "alive", Source: "test"}); err != nil {
		t.Fatal(err)
	}

	before := revisionCount(t, store, "m1")
	if err := store.Restore(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if got := revisionCount(t, store, "m1"); got != before {
		t.Fatalf("restore of active memory added revisions: %d -> %d", before, got)
	}

	store.now = clockAt(instant(2))
	if err := store.SoftDelete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	tombstoneCount := revisionCount(t, store, "m1")
	var pointerBefore string
	if err := store.DB().QueryRow(`SELECT current_revision_id FROM memories WHERE id = 'm1'`).Scan(&pointerBefore); err != nil {
		t.Fatal(err)
	}

	if err := store.Update(ctx, "m1", "must not resurrect", "decision"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateMetadata(ctx, "m1", map[string]any{"state": "must not mutate"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDelete(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	var pointerAfter string
	if err := store.DB().QueryRow(`SELECT current_revision_id FROM memories WHERE id = 'm1'`).Scan(&pointerAfter); err != nil {
		t.Fatal(err)
	}
	if pointerAfter != pointerBefore || revisionCount(t, store, "m1") != tombstoneCount {
		t.Fatal("active-only mutation changed a tombstoned memory")
	}
	if current, err := store.Get(ctx, "m1"); err != nil || current != nil {
		t.Fatalf("tombstoned memory resurrected: (%#v, %v)", current, err)
	}

	store.now = clockAt(instant(3))
	if err := store.Restore(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	restoredCount := revisionCount(t, store, "m1")
	if current, err := store.Get(ctx, "m1"); err != nil || current == nil || current.Content != "alive" {
		t.Fatalf("restore result = (%#v, %v)", current, err)
	}
	if err := store.Restore(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if got := revisionCount(t, store, "m1"); got != restoredCount {
		t.Fatalf("second restore added revisions: %d -> %d", restoredCount, got)
	}
}

func TestExplicitTemporalModesRequireExistingIdentity(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	_, err := store.SaveTemporal(ctx,
		Memory{Category: "fact", Content: "orphan", Source: "test"},
		TemporalWriteOptions{Mode: TemporalCorrect},
	)
	if !errors.Is(err, ErrTemporalIdentityRequired) {
		t.Fatalf("correction without identity error = %v", err)
	}
	_, err = store.SaveTemporal(ctx,
		Memory{ID: "missing", Category: "fact", Content: "orphan", Source: "test"},
		TemporalWriteOptions{Mode: TemporalTombstone, LogicalID: "missing"},
	)
	if !errors.Is(err, ErrTemporalIdentityRequired) {
		t.Fatalf("tombstone of missing identity error = %v", err)
	}
}

func newTemporalTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "temporal.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func instant(hour int) time.Time {
	return time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)
}

func clockAt(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func ptrTime(at time.Time) *time.Time {
	return &at
}

func metadataState(revision *MemoryRevision) string {
	if revision == nil {
		return ""
	}
	metadata, _ := revision.Memory.Metadata.(map[string]any)
	state, _ := metadata["state"].(string)
	return state
}

func revisionCount(t *testing.T, store *SQLiteStore, logicalID string) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE logical_id = ?`, logicalID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertRevisionContent(
	t *testing.T,
	store *SQLiteStore,
	logicalID string,
	validAt, knownAt time.Time,
	want string,
	wantTombstone bool,
) {
	t.Helper()
	revision, err := store.RevisionAt(context.Background(), logicalID, TemporalQueryOptions{
		ValidAt:          &validAt,
		KnownAt:          &knownAt,
		IncludeTombstone: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if revision == nil {
		t.Fatalf("no revision at valid=%s known=%s; want %q", validAt, knownAt, want)
	}
	if revision.Memory.Content != want || revision.IsTombstone != wantTombstone {
		t.Fatalf("revision at valid=%s known=%s = (%q, tombstone=%t), want (%q, %t)",
			validAt, knownAt, revision.Memory.Content, revision.IsTombstone, want, wantTombstone)
	}
}

func assertNoCurrentOverlap(t *testing.T, store *SQLiteStore, logicalID string) {
	t.Helper()
	tx, err := store.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := validateBitemporalIntervalsTx(context.Background(), tx, logicalID); err != nil {
		t.Fatalf("bitemporal intervals overlap: %v", err)
	}
}

func assertCurrentRevision(t *testing.T, db *sql.DB, id, logicalID, revisionID string) {
	t.Helper()
	var gotLogical, gotRevision string
	if err := db.QueryRow(`SELECT logical_id, current_revision_id FROM memories WHERE id = ?`, id).
		Scan(&gotLogical, &gotRevision); err != nil {
		t.Fatal(err)
	}
	if gotLogical != logicalID || gotRevision != revisionID {
		t.Fatalf("%s current identity = (%q, %q), want (%q, %q)",
			id, gotLogical, gotRevision, logicalID, revisionID)
	}
}

var preTemporalMigrationNames = []string{
	"001_initial_schema",
	"002_indexed_files",
	"003_content_hash_soft_delete",
	"004_cross_project_search",
	"005_live_sessions",
	"006_auto_capture",
	"007_dream_system",
	"008_content_optimizer",
	"009_content_project_id",
	"010_project_remote_key",
	"011_sync_metadata",
	"012_curation_state",
	"013_fts_multilingual_tokenizer",
	"014_artifact_store",
	"015_working_sets",
	"016_task_threads",
	"017_session_cockpit",
}

const legacyTemporalFixtureSchema = `
CREATE TABLE migrations (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE memories (
	id TEXT PRIMARY KEY,
	project_id TEXT,
	category TEXT NOT NULL,
	content TEXT NOT NULL,
	content_hash TEXT,
	keywords TEXT,
	embedding BLOB,
	source TEXT,
	source_id TEXT,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
	access_count INTEGER DEFAULT 0,
	last_accessed_at DATETIME,
	metadata TEXT,
	deleted_at DATETIME,
	sync_dirty BOOLEAN DEFAULT FALSE,
	sync_origin TEXT DEFAULT 'local',
	author TEXT,
	remote_project_key TEXT
);`
