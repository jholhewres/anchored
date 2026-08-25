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
// pruned memory, whatever the mechanism. memory_processing_jobs, remote_outbox,
// memory_revisions and dream_actions are deleted explicitly;
// memory_embedding_vectors is here precisely because it is NOT — it rides the
// FK cascade off memory_revisions, and this assertion is what proves that
// cascade actually fires. memories_fts is absent: the memories_fts_delete
// trigger owns it.
//
// Every table listed here MUST be populated by seedScopeMemory. A satellite the
// seed never fills makes assertNoOrphans vacuously true for it, which reads as
// coverage and is not.
var satelliteTables = []struct{ table, column string }{
	{"memory_processing_jobs", "memory_id"},
	{"remote_outbox", "memory_id"},
	{"memory_revisions", "memory_id"},
	{"memory_embedding_vectors", "memory_id"},
	{"dream_actions", "memory_id"},
	{"dream_actions", "related_memory_id"},
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
	// ProcessingJobs / RemoteOutbox are what put rows in those two satellites;
	// a plain Save() leaves them empty and makes the orphan check vacuous.
	if _, err := s.SaveTemporal(ctx, Memory{
		ID:        id,
		Category:  category,
		Content:   "content for " + id,
		Source:    "test-import",
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}, TemporalWriteOptions{
		ProcessingJobs: []ProcessingJobSpec{{Kind: "embedding", Generation: "gen-test"}},
		RemoteOutbox:   []RemoteOutboxSpec{{Remote: "remote-test", Payload: []byte(id)}},
	}); err != nil {
		t.Fatalf("seed memory %s: %v", id, err)
	}

	// dream_actions points at memories twice and, unlike memory_embedding_vectors,
	// has NO foreign key on either column — nothing cascades, so the prune has to
	// clear both explicitly. run_id is left NULL: its FK targets dream_runs and
	// is irrelevant to what this test pins.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, proposed_at, status)
		 VALUES ('act-'||?, NULL, ?, ?, 'dedup', 1.0, 'seeded', CURRENT_TIMESTAMP, 'proposed')`,
		id, id, id,
	); err != nil {
		t.Fatalf("seed dream action for %s: %v", id, err)
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
	for _, sat := range satelliteTables {
		// A satellite the seed never populated would pass trivially, so assert
		// it is non-empty before the prune runs is not possible here — instead
		// seedScopeMemory fills every one, and TestSeedFillsEverySatellite pins
		// that contract.
		n := countRows(t, s.db, "SELECT COUNT(*) FROM "+sat.table+
			" WHERE "+sat.column+" IS NOT NULL AND "+sat.column+" NOT IN (SELECT id FROM memories)")
		if n != 0 {
			t.Errorf("%s.%s left %d orphan row(s) after hard prune", sat.table, sat.column, n)
		}
	}
}

// The orphan assertion is only worth anything if the seed actually writes to
// every satellite it checks. This pins that: a satellite added to the list
// without a matching seed write fails here rather than silently passing
// assertNoOrphans for the rest of the file's lifetime.
func TestSeedFillsEverySatellite(t *testing.T) {
	s := newScopeTestStore(t)
	seedScopeMemory(t, s, "seeded-1", "technical", time.Now().UTC().AddDate(0, 0, -10))
	for _, sat := range satelliteTables {
		n := countRows(t, s.db, "SELECT COUNT(*) FROM "+sat.table+
			" WHERE "+sat.column+" = 'seeded-1'")
		if n == 0 {
			t.Errorf("seedScopeMemory writes nothing to %s.%s — assertNoOrphans is vacuous for it",
				sat.table, sat.column)
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
	satelliteBefore := map[string]int{}
	for _, sat := range satelliteTables {
		satelliteBefore[sat.table] = countRows(t, s.db, "SELECT COUNT(*) FROM "+sat.table)
	}

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
	// Every satellite must be untouched, not just the one the old assertion
	// happened to name — the table filter used to sit inside this condition, so
	// three of four iterations computed a count and threw it away.
	for _, sat := range satelliteTables {
		if got := countRows(t, s.db, "SELECT COUNT(*) FROM "+sat.table); got != satelliteBefore[sat.table] {
			t.Errorf("dry run touched %s: %d rows, want %d",
				sat.table, got, satelliteBefore[sat.table])
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

// Every other test in this file runs Hard:true, so this covers the soft path's
// success case plus its two modifiers.
func TestDeleteByScopeSoftTombstonesAndKeepsRows(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -120)

	seedScopeMemory(t, s, "soft-1", "technical", old)
	seedScopeMemory(t, s, "soft-2", "technical", old)
	seedScopeMemory(t, s, "keep-1", "decision", old)

	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{Category: "technical"})
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if n != 2 {
		t.Fatalf("soft-deleted %d, want 2", n)
	}
	// A soft delete tombstones: the rows stay, they just leave the active view.
	if got := countRows(t, s.db, "SELECT COUNT(*) FROM memories"); got != 3 {
		t.Errorf("rows physically removed by a SOFT delete: %d left, want 3", got)
	}
	if got := countRows(t, s.db, "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL"); got != 1 {
		t.Errorf("active memories = %d, want 1", got)
	}
}

func TestDeleteByScopeSoftSkipsAlreadyTombstoned(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -120)
	seedScopeMemory(t, s, "soft-1", "technical", old)

	if _, err := s.DeleteByScope(ctx, DeleteScopeOptions{Category: "technical"}); err != nil {
		t.Fatalf("first soft delete: %v", err)
	}
	// includeDeleted is tied to Hard: a second soft pass must find nothing left
	// to do, while a hard pass still reclaims the tombstone.
	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{Category: "technical"})
	if err != nil {
		t.Fatalf("second soft delete: %v", err)
	}
	if n != 0 {
		t.Errorf("second soft pass reported %d, want 0 — tombstones are not work", n)
	}

	hard, err := s.DeleteByScope(ctx, DeleteScopeOptions{Category: "technical", Hard: true})
	if err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if hard != 1 {
		t.Errorf("hard pass reclaimed %d, want 1 — it must include tombstones", hard)
	}
	assertNoOrphans(t, s)
}

func TestDeleteByScopeSoftHonorsOlderThanAndLimit(t *testing.T) {
	s := newScopeTestStore(t)
	ctx := context.Background()

	seedScopeMemory(t, s, "old-1", "plan", time.Now().UTC().AddDate(0, 0, -90))
	seedScopeMemory(t, s, "old-2", "plan", time.Now().UTC().AddDate(0, 0, -80))
	seedScopeMemory(t, s, "new-1", "plan", time.Now().UTC().AddDate(0, 0, -5))

	n, err := s.DeleteByScope(ctx, DeleteScopeOptions{
		Category:  "plan",
		OlderThan: time.Now().UTC().AddDate(0, 0, -60),
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("DeleteByScope: %v", err)
	}
	if n != 1 {
		t.Fatalf("soft-deleted %d, want 1 (limit)", n)
	}
	var survivors []string
	rows, err := s.db.Query("SELECT id FROM memories WHERE deleted_at IS NULL ORDER BY id")
	if err != nil {
		t.Fatalf("query survivors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		survivors = append(survivors, id)
	}
	if len(survivors) != 2 || survivors[0] != "new-1" || survivors[1] != "old-2" {
		t.Errorf("survivors = %v, want [new-1 old-2] — the cap must take the oldest first", survivors)
	}
}
