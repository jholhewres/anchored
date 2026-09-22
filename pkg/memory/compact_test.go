package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// seedRedundantHistory reproduces what the unguarded save path produced: one
// memory whose current revision is preceded by N revisions repeating the same
// content_hash and metadata, each dragging a copy of the embedding vector.
func seedRedundantHistory(t *testing.T, store *SQLiteStore, extra int) (memoryID string, current string) {
	t.Helper()
	ctx := context.Background()
	svc := &Service{store: store, logger: discardLogger(), embedSem: make(chan struct{}, 1), shutdown: make(chan struct{})}

	m, err := svc.Save(ctx, "a fact that gets re-saved over and over", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DB().QueryRow(
		`SELECT current_revision_id FROM memories WHERE id = ?`, m.ID).Scan(&current); err != nil {
		t.Fatal(err)
	}

	var contentHash sql.NullString
	var metadata sql.NullString
	var logicalID string
	var validFrom, systemFrom int64
	if err := store.DB().QueryRow(
		`SELECT content_hash, metadata, logical_id, valid_from, system_from
		 FROM memory_revisions WHERE revision_id = ?`, current,
	).Scan(&contentHash, &metadata, &logicalID, &validFrom, &systemFrom); err != nil {
		t.Fatal(err)
	}

	// No embedder runs in this service, so stand in for one: a generation plus
	// the single document vector the current revision would own.
	const genID = "gen_test"
	if _, err := store.DB().Exec(`INSERT INTO embedding_generations (
		generation_id, semantic_space_id, provider, model, dimensions,
		normalization, state, snapshot_at, created_at, activated_at
	) VALUES (?, 'es_test', 'test', 'm', 2, 'l2', 'active', ?, ?, ?)`,
		genID, systemFrom, systemFrom, systemFrom); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO memory_embedding_vectors (
		revision_id, memory_id, generation_id, semantic_space_id, purpose,
		provider, model, dimensions, normalization, content_hash, embedding, embedded_at
	) VALUES (?, ?, ?, 'es_test', 'document', 'test', 'm', 2, 'l2', ?, X'0000803F0000803F', ?)`,
		current, m.ID, genID, contentHash, systemFrom); err != nil {
		t.Fatal(err)
	}

	// The duplicates predate the current revision in system time, so the
	// current one is never the group's earliest entry.
	for i := 0; i < extra; i++ {
		rid := newUUID()
		_, err := store.DB().Exec(`INSERT INTO memory_revisions (
			revision_id, memory_id, logical_id, category, content, content_hash,
			metadata, memory_created_at, memory_updated_at, temporal_mode,
			is_tombstone, valid_from, system_from, system_to
		) VALUES (?, ?, ?, 'fact', ?, ?, ?, ?, ?, 'supersede', FALSE, ?, ?, ?)`,
			rid, m.ID, logicalID, m.Content, contentHash, metadata,
			time.Now().UTC(), time.Now().UTC(),
			validFrom, systemFrom-int64(extra-i)-1, systemFrom-int64(extra-i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().Exec(`INSERT INTO memory_embedding_vectors (
			revision_id, memory_id, generation_id, semantic_space_id, purpose,
			provider, model, dimensions, normalization, content_hash, embedding, embedded_at
		) SELECT ?, memory_id, generation_id, semantic_space_id, purpose, provider,
			model, dimensions, normalization, content_hash, embedding, embedded_at
		  FROM memory_embedding_vectors WHERE revision_id = ?`, rid, current); err != nil {
			t.Fatal(err)
		}
	}
	return m.ID, current
}

func openCompactStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "compact.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestCompactRemovesRedundantRevisionsAndKeepsCurrent(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	memoryID, current := seedRedundantHistory(t, store, 20)

	stats, err := Compact(ctx, store.DB(), CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The earliest entry of the group survives — it records when the state
	// began — so 20 duplicates collapse to that one plus the current revision.
	if stats.RedundantRevisions != 19 {
		t.Fatalf("redundant revisions removed = %d, want 19", stats.RedundantRevisions)
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("revisions left = %d, want the earliest plus the current one", n)
	}

	var survives int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE revision_id = ?`, current).Scan(&survives); err != nil {
		t.Fatal(err)
	}
	if survives != 1 {
		t.Fatal("the current revision must survive compaction")
	}

	// The memory itself is untouched and still readable.
	got, err := store.Get(ctx, memoryID)
	if err != nil || got == nil {
		t.Fatalf("memory lost: %v", err)
	}
}

func TestCompactDryRunChangesNothing(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	memoryID, _ := seedRedundantHistory(t, store, 7)

	stats, err := Compact(ctx, store.DB(), CompactOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RedundantRevisions != 6 {
		t.Fatalf("dry run reported %d, want 6", stats.RedundantRevisions)
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("dry run deleted rows: revisions = %d, want 8", n)
	}
}

func TestCompactKeepHistoryPrunesOnlyDerivedData(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	memoryID, _ := seedRedundantHistory(t, store, 5)

	stats, err := Compact(ctx, store.DB(), CompactOptions{KeepHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.RedundantRevisions != 0 {
		t.Fatalf("keep-history removed %d revisions, want 0", stats.RedundantRevisions)
	}
	if stats.SupersededVectors != 5 {
		t.Fatalf("superseded vectors removed = %d, want 5", stats.SupersededVectors)
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("revisions = %d, want all 6 preserved", n)
	}
}

// The vector attached to the current revision is the only one search reads; it
// must never be swept.
func TestCompactKeepsCurrentRevisionVector(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	_, current := seedRedundantHistory(t, store, 12)

	if _, err := Compact(ctx, store.DB(), CompactOptions{}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_embedding_vectors WHERE revision_id = ?`, current).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("current revision vectors = %d, want 1", n)
	}
}

func TestCompactIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	seedRedundantHistory(t, store, 9)

	if _, err := Compact(ctx, store.DB(), CompactOptions{}); err != nil {
		t.Fatal(err)
	}
	second, err := Compact(ctx, store.DB(), CompactOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.RedundantRevisions != 0 || second.SupersededVectors != 0 {
		t.Fatalf("second pass removed %+v, want nothing", second)
	}
}

// A revision an undelivered envelope still points at must survive, or the
// pending sync loses its anchor.
func TestCompactKeepsRevisionsWithPendingOutbox(t *testing.T) {
	ctx := context.Background()
	store := openCompactStore(t)
	memoryID, _ := seedRedundantHistory(t, store, 4)

	var pinned string
	if err := store.DB().QueryRow(
		`SELECT r.revision_id FROM memory_revisions r
		 WHERE r.memory_id = ?
		   AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.current_revision_id = r.revision_id)
		 ORDER BY r.system_from DESC LIMIT 1`, memoryID).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO remote_outbox (
		operation_id, memory_id, revision_id, remote, project, payload_hash,
		payload_snapshot, state, created_at, updated_at
	) VALUES (?, ?, ?, 'default', 'p', 'h', X'00', 'pending', ?, ?)`,
		newUUID(), memoryID, pinned, time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}

	if _, err := Compact(ctx, store.DB(), CompactOptions{}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE revision_id = ?`, pinned).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("a revision with a pending outbox envelope must survive")
	}
}
