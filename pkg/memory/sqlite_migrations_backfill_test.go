package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestBackfillLedgerOrphans_AdoptsRowsWithoutRevision covers the rows a write
// path left outside the temporal ledger after migration 018 had already run:
// curation resolves a memory through memories.current_revision_id and the
// embedding queue joins on it, so an orphan is invisible to both.
func TestBackfillLedgerOrphans_AdoptsRowsWithoutRevision(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "backfill.db"), nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	db := store.DB()
	ctx := context.Background()

	// Reproduce both orphan shapes: an active row and a soft-deleted one.
	for _, o := range []struct{ id, deletedAt string }{
		{"orphan-active", ""},
		{"orphan-deleted", "2026-09-01 10:00:00"},
	} {
		var deleted any
		if o.deletedAt != "" {
			deleted = o.deletedAt
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO memories
			 (id, category, content, content_hash, keywords, source,
			  created_at, updated_at, access_count, metadata, sync_dirty, deleted_at)
			 VALUES (?, 'learning', 'conteudo orfao', 'hash', '[]', 'stop_hook',
			         '2026-08-27 12:00:00', '2026-08-27 12:00:00', 0, '{}', 0, ?)`,
			o.id, deleted,
		); err != nil {
			t.Fatalf("insert orphan %s: %v", o.id, err)
		}
	}

	orphans := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories
			 WHERE current_revision_id IS NULL OR current_revision_id = ''`,
		).Scan(&n); err != nil {
			t.Fatalf("count orphans: %v", err)
		}
		return n
	}

	if got := orphans(); got != 2 {
		t.Fatalf("orphans before backfill = %d, want 2", got)
	}

	if _, err := db.ExecContext(ctx, migrationBackfillLedgerOrphans); err != nil {
		t.Fatalf("apply backfill: %v", err)
	}
	if got := orphans(); got != 0 {
		t.Errorf("orphans after backfill = %d, want 0", got)
	}

	// The active row gets an open base revision; the deleted one a tombstone.
	for _, want := range []struct {
		id, mode  string
		tombstone bool
	}{
		{"orphan-active", "supersede", false},
		{"orphan-deleted", "tombstone", true},
	} {
		var mode, logicalID string
		var tombstone bool
		var systemTo sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT r.temporal_mode, r.logical_id, r.is_tombstone, r.system_to
			 FROM memories m JOIN memory_revisions r ON r.revision_id = m.current_revision_id
			 WHERE m.id = ?`, want.id,
		).Scan(&mode, &logicalID, &tombstone, &systemTo); err != nil {
			t.Fatalf("resolve revision for %s: %v", want.id, err)
		}
		if mode != want.mode {
			t.Errorf("%s: temporal_mode = %q, want %q", want.id, mode, want.mode)
		}
		if tombstone != want.tombstone {
			t.Errorf("%s: is_tombstone = %v, want %v", want.id, tombstone, want.tombstone)
		}
		if logicalID != want.id {
			t.Errorf("%s: logical_id = %q, want %q", want.id, logicalID, want.id)
		}
		if systemTo.Valid {
			t.Errorf("%s: system_to must stay open", want.id)
		}
	}

	// Idempotent: the backfill runs inside a migration that may be replayed on
	// a database that already has revisions for every row.
	if _, err := db.ExecContext(ctx, migrationBackfillLedgerOrphans); err != nil {
		t.Fatalf("re-apply backfill: %v", err)
	}
	var revisions int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = 'orphan-active'`,
	).Scan(&revisions); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if revisions != 1 {
		t.Errorf("revisions for orphan-active = %d, want 1", revisions)
	}
}
