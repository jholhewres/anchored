package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	ctxpkg "github.com/jholhewres/anchored/pkg/context"
)

// newDedupTestDB opens an in-memory DB with the session_events schema.
func newDedupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(ctxpkg.MigrationSQL); err != nil {
		t.Fatalf("migration: %v", err)
	}
	if _, err := db.Exec(ctxpkg.MigrationSQL009); err != nil {
		t.Fatalf("migration 009: %v", err)
	}
	return db
}

// fireIdenticalPostToolUse records the same tool call n times. `seq` is shared
// across calls within a test so every event gets a unique id — only dedup (not
// a primary-key collision) can keep the row count down.
func fireIdenticalPostToolUse(t *testing.T, db *sql.DB, seq *int, n int) {
	t.Helper()
	for k := 0; k < n; k++ {
		*seq++
		id := fmt.Sprintf("evt-%d", *seq)
		recordPostToolUseEvent(PostToolUseDeps{
			Stdin:          strings.NewReader(`{"session_id":"sess-dup","tool_name":"Read","tool_input":{"file_path":"/x/y.go"},"tool_response":"ok"}`),
			Stdout:         &strings.Builder{},
			DB:             db,
			ResolveProject: func(string) string { return "" },
			NewID:          func() string { return id },
			Logger:         nilDebugLogger(),
		})
	}
}

func countSessionEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM session_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Two identical PostToolUse events within the 5-min window collapse to one row.
func TestPostToolUse_DedupWithinWindow(t *testing.T) {
	db := newDedupTestDB(t)
	seq := 0
	fireIdenticalPostToolUse(t, db, &seq, 2)
	if n := countSessionEvents(t, db); n != 1 {
		t.Fatalf("expected 1 row after 2 identical events <5min apart, got %d", n)
	}
}

// After backdating the first event past the window, an identical event is a
// genuine new record → 2 rows.
func TestPostToolUse_NoDedupPastWindow(t *testing.T) {
	db := newDedupTestDB(t)
	seq := 0
	fireIdenticalPostToolUse(t, db, &seq, 1)
	if n := countSessionEvents(t, db); n != 1 {
		t.Fatalf("setup: expected 1 row, got %d", n)
	}
	// Push the only row 6 minutes into the past so it's outside the window.
	if _, err := db.Exec(`UPDATE session_events SET created_at = datetime('now','-6 minutes')`); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	fireIdenticalPostToolUse(t, db, &seq, 1)
	if n := countSessionEvents(t, db); n != 2 {
		t.Fatalf("expected 2 rows when the prior event is outside the window, got %d", n)
	}
}
