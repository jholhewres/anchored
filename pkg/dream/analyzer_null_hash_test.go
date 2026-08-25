package dream

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// content_hash is nullable, and rows predating the hashing backfill carry NULL.
// Scanning that into a plain string aborted the entire dream run with
// "converting NULL to string is unsupported", so a single unhashed memory
// disabled consolidation for the whole corpus.
func TestAnalyzeToleratesNullContentHash(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "dream.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE memories (
		id TEXT PRIMARY KEY,
		project_id TEXT,
		category TEXT,
		content TEXT,
		content_hash TEXT,
		keywords TEXT,
		embedding BLOB,
		created_at DATETIME,
		deleted_at DATETIME
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	rows := []struct {
		id, content string
		hash        any
	}{
		{"m-null", "memoria sem hash", nil},
		{"m-dup-a", "conteudo repetido", "hash-repetido"},
		{"m-dup-b", "conteudo repetido", "hash-repetido"},
		{"m-solo", "conteudo unico", "hash-unico"},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO memories (id, content, content_hash, created_at) VALUES (?, ?, ?, '2026-01-01 00:00:00')`,
			r.id, r.content, r.hash,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}

	report, err := NewAnalyzer(db, nil, DreamConfig{}, nil).Analyze(context.Background())
	if err != nil {
		t.Fatalf("Analyze aborted on a NULL content_hash: %v", err)
	}
	if report.TotalMemories != 4 {
		t.Errorf("TotalMemories = %d, want 4", report.TotalMemories)
	}
	// The unhashed row must be skipped by exact dedup, not treated as matching
	// the other unhashed rows — collapsing them under "" would be worse than
	// the crash.
	if report.ExactDupes != 1 {
		t.Errorf("ExactDupes = %d, want 1 (only the real hash pair)", report.ExactDupes)
	}
}
