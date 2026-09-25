package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// The current view is written by an upsert that always sets content, so an
// "AFTER UPDATE OF content" trigger fired on every tombstone, restore and
// re-save and rewrote the FTS index for text that had not changed. Archiving
// tens of thousands of memories would rewrite the index tens of thousands of
// times.
func TestFTSIndexUntouchedWhenContentDoesNotChange(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fts.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	if _, err := store.SaveTemporal(ctx, Memory{ID: "m1", Category: "fact", Content: "full text search stays put"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}

	ftsState := func() (rows int, bytes int) {
		t.Helper()
		if err := store.DB().QueryRow(
			"SELECT COUNT(*), COALESCE(SUM(LENGTH(block)), 0) FROM memories_fts_data",
		).Scan(&rows, &bytes); err != nil {
			t.Fatal(err)
		}
		return rows, bytes
	}
	beforeRows, beforeBytes := ftsState()

	for i := 0; i < 5; i++ {
		if err := store.SoftDelete(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
		if err := store.Restore(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
	}

	if rows, bytes := ftsState(); rows != beforeRows || bytes != beforeBytes {
		t.Errorf("FTS index rewritten without a content change: rows %d -> %d, bytes %d -> %d",
			beforeRows, rows, beforeBytes, bytes)
	}

	// A real content change must still reach the index.
	if err := store.Update(ctx, "m1", "rewritten text about sqlite", "fact"); err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'sqlite'").Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("content change must be indexed, got %d hits", hits)
	}
}

// Existing databases carry the unconditional trigger from before 024; the
// migration must replace it on upgrade, not only fresh installs.
func TestMigration024_ReplacesTheOldTriggerOnUpgrade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Roll the database back to its pre-024 shape.
	if _, err := store.DB().Exec(`
		DROP TRIGGER memories_fts_update;
		CREATE TRIGGER memories_fts_update AFTER UPDATE OF content, keywords ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content, keywords) VALUES('delete', old.rowid, old.content, old.keywords);
			INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords);
		END;
		DELETE FROM migrations WHERE name = '024_fts_update_only_on_change';`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveTemporal(ctx, Memory{ID: "m1", Category: "fact", Content: "upgraded text"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = NewSQLiteStore(path, nil) // the new binary opens it
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var before, after int
	count := func() int {
		var n int
		if err := store.DB().QueryRow("SELECT COUNT(*) FROM memories_fts_data").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before = count()
	for i := 0; i < 3; i++ {
		if err := store.SoftDelete(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
		if err := store.Restore(ctx, "m1"); err != nil {
			t.Fatal(err)
		}
	}
	if after = count(); after != before {
		t.Errorf("upgraded database still rewrites FTS on unchanged content: %d -> %d rows", before, after)
	}
}

func TestFTSIndexFollowsAKeywordsOnlyChange(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "keywords.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.SaveTemporal(context.Background(), Memory{ID: "m1", Category: "fact", Content: "content stays"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE memories SET keywords = '["quetzalword"]' WHERE id = 'm1'`); err != nil {
		t.Fatal(err)
	}
	var hits int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH 'quetzalword'").Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("a keywords-only change must reach the index, got %d hits", hits)
	}
}
