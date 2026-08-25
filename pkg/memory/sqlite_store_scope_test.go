package memory

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// satelliteTables are every memory-keyed table that must end up empty of a
// pruned memory, whatever the mechanism. memory_processing_jobs, remote_outbox
// and memory_revisions are deleted explicitly; memory_embedding_vectors is here
// precisely because it is NOT — it rides the FK cascade off memory_revisions,
// and this assertion is what proves that cascade actually fires.
// memories_fts is absent: the memories_fts_delete trigger owns it.
var satelliteTables = []string{
	"memory_processing_jobs",
	"remote_outbox",
	"memory_revisions",
	"memory_embedding_vectors",
}

func newScopeTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(
		filepath.Join(t.TempDir(), "scope.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func seedScopeMemory(t *testing.T, s *SQLiteStore, id, category string, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.Save(ctx, Memory{
		ID:        id,
		Category:  category,
		Content:   "content for " + id,
		Source:    "test-import",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}); err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO embedding_generations
		   (generation_id, semantic_space_id, provider, model, model_revision,
		    dimensions, normalization, state, snapshot_at, created_at)
		 VALUES ('gen-test', 'space-test', 'onnx', 'test-model', 'r1', 3, 'l2',
		         'building', 0, 0)`,
	); err != nil {
		t.Fatalf("seed embedding generation: %v", err)
	}

	// The vector hangs off the revision, not the memory, so it needs the real
	// revision_id Save() minted — which is also exactly why deleting revisions
	// is what reclaims vectors.
	var revisionID string
	if err := s.db.QueryRowContext(ctx,
		"SELECT revision_id FROM memory_revisions WHERE memory_id = ? ORDER BY created_at DESC LIMIT 1", id,
	).Scan(&revisionID); err != nil {
		t.Fatalf("read revision for %s: %v", id, err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_embedding_vectors
		   (revision_id, memory_id, generation_id, semantic_space_id, purpose,
		    provider, model, model_revision, dimensions, normalization,
		    content_hash, embedding, embedded_at)
		 VALUES (?, ?, 'gen-test', 'space-test', 'document', 'onnx', 'test-model',
		         'r1', 3, 'l2', 'hash-'||?, X'00', 0)`,
		revisionID, id, id,
	); err != nil {
		t.Fatalf("seed embedding vector for %s: %v", id, err)
	}
}

func countRows(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func assertNoOrphans(t *testing.T, s *SQLiteStore) {
	t.Helper()
	for _, table := range satelliteTables {
		n := countRows(t, s.db,
			"SELECT COUNT(*) FROM "+table+" WHERE memory_id NOT IN (SELECT id FROM memories)")
		if n != 0 {
			t.Errorf("%s left %d orphan row(s) after hard prune", table, n)
		}
	}
}

func TestDeleteByScopeHardLeavesNoOrphans(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -120)

	seedScopeMemory(t, s, "tech-1", "technical", old)
	seedScopeMemory(t, s, "tech-2", "technical", old)
	seedScopeMemory(t, s, "keep-1", "decision", old)

	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{Category: "technical", Hard: true})
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d memories, want 2", n)
	}

	assertNoOrphans(t, s)

	if got := countRows(t, s.db, "SELECT COUNT(*) FROM memories"); got != 1 {
		t.Errorf("memories left = %d, want 1", got)
	}
	// The survivor must keep its satellite rows: the prune has to be scoped,
	// not a table-wide sweep that happens to pass the orphan check.
	if got := countRows(t, s.db,
		"SELECT COUNT(*) FROM memory_embedding_vectors WHERE memory_id = 'keep-1'"); got != 1 {
		t.Errorf("survivor lost its embedding vector (got %d rows, want 1)", got)
	}
}

func TestDeleteByScopeDryRunTouchesNothing(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -120)

	seedScopeMemory(t, s, "tech-1", "technical", old)
	seedScopeMemory(t, s, "tech-2", "technical", old)

	before := countRows(t, s.db, "SELECT COUNT(*) FROM memories")
	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{
		Category: "technical", Hard: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("DeleteByScope dry run: %v", err)
	}
	if n != 2 {
		t.Errorf("dry run counted %d, want 2", n)
	}
	if after := countRows(t, s.db, "SELECT COUNT(*) FROM memories"); after != before {
		t.Errorf("dry run changed the corpus: %d -> %d", before, after)
	}
	for _, table := range satelliteTables {
		if got := countRows(t, s.db, "SELECT COUNT(*) FROM "+table); table == "memory_embedding_vectors" && got != 2 {
			t.Errorf("dry run touched %s (got %d rows, want 2)", table, got)
		}
	}
}

func TestDeleteByScopeOlderThanSparesRecent(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()

	seedScopeMemory(t, s, "old-1", "plan", time.Now().UTC().AddDate(0, 0, -90))
	seedScopeMemory(t, s, "new-1", "plan", time.Now().UTC().AddDate(0, 0, -5))

	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{
		Category:  "plan",
		Hard:      true,
		OlderThan: time.Now().UTC().AddDate(0, 0, -60),
	})
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed %d memories, want 1", n)
	}
	var survivor string
	if err := s.db.QueryRow("SELECT id FROM memories").Scan(&survivor); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if survivor != "new-1" {
		t.Errorf("survivor = %q, want new-1", survivor)
	}
	assertNoOrphans(t, s)
}

func TestDeleteByScopeLimitDrainsOldestFirst(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()

	seedScopeMemory(t, s, "oldest", "technical", time.Now().UTC().AddDate(0, 0, -300))
	seedScopeMemory(t, s, "middle", "technical", time.Now().UTC().AddDate(0, 0, -200))
	seedScopeMemory(t, s, "newest", "technical", time.Now().UTC().AddDate(0, 0, -100))

	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{
		Category: "technical", Hard: true, Limit: 2,
	})
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d memories, want 2", n)
	}
	var survivor string
	if err := s.db.QueryRow("SELECT id FROM memories").Scan(&survivor); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if survivor != "newest" {
		t.Errorf("survivor = %q, want newest — a capped run must drain oldest-first", survivor)
	}
	assertNoOrphans(t, s)
}

func TestDeleteByScopeRequiresACondition(t *testing.T) {
	s := newScopeTestStore(t)
	if _, err := s.DeleteByScope(context.Background(), DeleteScopeOptions{Hard: true}); err == nil {
		t.Fatal("an unscoped hard delete must be refused, not treated as delete-everything")
	}
}
