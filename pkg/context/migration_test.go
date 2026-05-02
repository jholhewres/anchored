package ctx

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var count int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table,
	).Scan(&count)
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return count > 0
}

func TestMigration008_FreshDB(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	for _, table := range []string{"content_chunks", "session_events", "content_vocabulary"} {
		if !tableExists(t, db, table) {
			t.Errorf("table %s not created", table)
		}
	}

	var count int
	err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='content_chunks_fts'").Scan(&count)
	if err != nil {
		t.Fatalf("check fts table: %v", err)
	}
	if count == 0 {
		t.Error("content_chunks_fts virtual table not created")
	}
}

func TestMigration008_ExistingDB(t *testing.T) {
	db := openTestDB(t)

	schema := `CREATE TABLE IF NOT EXISTS memories (
		id TEXT PRIMARY KEY,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create base schema: %v", err)
	}

	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("apply migration on existing DB: %v", err)
	}

	if !tableExists(t, db, "content_chunks") {
		t.Error("content_chunks table not created alongside existing tables")
	}
	if !tableExists(t, db, "memories") {
		t.Error("existing memories table should still exist")
	}
}

func TestMigration008_TrigramTokenizer(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	_, err := db.Exec(
		"INSERT INTO content_chunks (id, source, content) VALUES (?, ?, ?)",
		"test-1", "execute", "hello world example text",
	)
	if err != nil {
		t.Fatalf("insert test chunk: %v", err)
	}

	var count int
	err = db.QueryRow(
		"SELECT count(*) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?",
		"example",
	).Scan(&count)
	if err != nil {
		t.Fatalf("fts trigram search: %v", err)
	}
	if count != 1 {
		t.Errorf("trigram match for 'example': got %d, want 1", count)
	}

	var snippet string
	err = db.QueryRow(
		"SELECT snippet(content_chunks_fts, 0, '>>>', '<<<', '...', 10) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?",
		"world",
	).Scan(&snippet)
	if err != nil {
		t.Fatalf("fts snippet: %v", err)
	}
	if snippet == "" {
		t.Error("expected non-empty snippet from trigram FTS5")
	}
}

func TestMigration008_Triggers(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	_, err := db.Exec(
		"INSERT INTO content_chunks (id, source, content) VALUES (?, ?, ?)",
		"trig-1", "execute", "initial content",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count int
	db.QueryRow("SELECT count(*) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?", "initial").Scan(&count)
	if count != 1 {
		t.Fatalf("after insert: got %d rows in FTS, want 1", count)
	}

	_, err = db.Exec("UPDATE content_chunks SET content = ? WHERE id = ?", "updated content", "trig-1")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	db.QueryRow("SELECT count(*) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?", "updated").Scan(&count)
	if count != 1 {
		t.Errorf("after update: got %d rows matching 'updated', want 1", count)
	}
	db.QueryRow("SELECT count(*) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?", "initial").Scan(&count)
	if count != 0 {
		t.Errorf("after update: got %d rows matching 'initial', want 0", count)
	}

	_, err = db.Exec("DELETE FROM content_chunks WHERE id = ?", "trig-1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	db.QueryRow("SELECT count(*) FROM content_chunks_fts WHERE content_chunks_fts MATCH ?", "updated").Scan(&count)
	if count != 0 {
		t.Errorf("after delete: got %d rows in FTS, want 0", count)
	}
}
