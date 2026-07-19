package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newRecallTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := ensureRecallSchema(db); err != nil {
		t.Fatalf("ensureRecallSchema: %v", err)
	}
	return db
}

func TestRecordRecall_And_Summary(t *testing.T) {
	db := newRecallTestDB(t)

	recordRecall(db, "proj-1", "sessionstart", 1200, 9000)
	recordRecall(db, "proj-1", "userpromptsubmit", 300, 9000)
	recordRecall(db, "proj-1", "userpromptsubmit", 0, 9000) // skipped: no tokens injected

	s, err := queryRecallSummary(db, 7)
	if err != nil {
		t.Fatalf("queryRecallSummary: %v", err)
	}
	if s.Injections != 2 {
		t.Fatalf("injections = %d, want 2 (zero-token recall skipped)", s.Injections)
	}
	if s.InjectedTokens != 1500 {
		t.Fatalf("injected = %d, want 1500", s.InjectedTokens)
	}
	if s.BaselineTokens != 18000 {
		t.Fatalf("baseline = %d, want 18000", s.BaselineTokens)
	}
	// saved = 18000-1500 = 16500 of 18000 → 91.66%
	if got := s.SavingsPct(); got < 91.0 || got > 92.0 {
		t.Fatalf("savings = %.2f, want ~91.7", got)
	}
}

func TestSavingsPct_NoBaseline(t *testing.T) {
	if got := (recallTokenSummary{InjectedTokens: 500}).SavingsPct(); got != 0 {
		t.Fatalf("no-baseline savings = %.2f, want 0", got)
	}
}

func TestProjectBaselineTokens_ComputesAndCaches(t *testing.T) {
	db := newRecallTestDB(t)
	dir := t.TempDir()
	// A CLAUDE.md and a nested SKILL.md should both count toward the baseline.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("some project rules here"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, ".claude", "skills", "deploy")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("deploy skill body"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	got := projectBaselineTokens(db, "proj-x", dir, now)
	if got <= 0 {
		t.Fatalf("baseline = %d, want > 0", got)
	}

	// Second call the same day must hit the cache even if files change.
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("expanded rules "+string(make([]byte, 400))), 0o644); err != nil {
		t.Fatal(err)
	}
	cached := projectBaselineTokens(db, "proj-x", dir, now)
	if cached != got {
		t.Fatalf("same-day recompute: got %d, want cached %d", cached, got)
	}

	// A later day recomputes and picks up the larger file.
	tomorrow := now.Add(24 * time.Hour)
	recomputed := projectBaselineTokens(db, "proj-x", dir, tomorrow)
	if recomputed <= got {
		t.Fatalf("next-day recompute = %d, want > %d", recomputed, got)
	}
}
