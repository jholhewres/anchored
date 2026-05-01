package stack

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type mockDB struct {
	db *sql.DB
}

func (m *mockDB) DB() *sql.DB { return m.db }

func setupTestDB(t *testing.T) *mockDB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			category TEXT NOT NULL,
			content TEXT NOT NULL,
			access_count INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS essential_stories (
			project_id TEXT PRIMARY KEY,
			story TEXT,
			generated_at DATETIME,
			bytes INTEGER,
			schema_version INTEGER DEFAULT 1
		);
	`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	return &mockDB{db: db}
}

func insertMemory(t *testing.T, db *sql.DB, projectID, category, content string, accessCount int) {
	t.Helper()
	_, err := db.Exec(
		"INSERT INTO memories (id, project_id, category, content, access_count) VALUES (?, ?, ?, ?, ?)",
		"mem-"+content, projectID, category, content, accessCount,
	)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
}

func TestEssentialLayer_Render_EmptyProject(t *testing.T) {
	accessor := setupTestDB(t)
	layer := NewEssentialLayer(accessor, nil)

	result := layer.Render("nonexistent")
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func TestEssentialLayer_Render_EmptyProjectID(t *testing.T) {
	accessor := setupTestDB(t)
	layer := NewEssentialLayer(accessor, nil)

	result := layer.Render("")
	if result != "" {
		t.Errorf("expected empty for empty projectID, got %q", result)
	}
}

func TestEssentialLayer_Render_BuildsMarkdown(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	insertMemory(t, db, "proj1", "fact", "Go uses garbage collection", 10)
	insertMemory(t, db, "proj1", "fact", "Project uses SQLite", 8)
	insertMemory(t, db, "proj1", "decision", "Chose WAL mode for concurrency", 12)
	insertMemory(t, db, "proj1", "decision", "Use stack-based memory layers", 7)
	insertMemory(t, db, "proj1", "event", "Sprint started", 5)
	insertMemory(t, db, "proj1", "event", "Deployed v1.0", 3)
	insertMemory(t, db, "proj1", "preference", "Prefer small commits", 6)
	insertMemory(t, db, "proj1", "preference", "Prefer Go over Rust", 4)

	layer := NewEssentialLayer(accessor, nil)
	result := layer.Render("proj1")

	if result == "" {
		t.Fatal("expected non-empty result")
	}

	expectedParts := []string{
		"## Project Context",
		"### Key Facts & Decisions",
		"Chose WAL mode for concurrency",
		"Go uses garbage collection",
		"### Recent Events",
		"Sprint started",
		"### Preferences",
		"Prefer small commits",
	}
	for _, part := range expectedParts {
		if !contains(result, part) {
			t.Errorf("missing %q in result:\n%s", part, result)
		}
	}
}

func TestEssentialLayer_Render_CacheHit(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	now := time.Now().UTC()
	_, err := db.Exec(
		"INSERT INTO essential_stories (project_id, story, generated_at, bytes) VALUES (?, ?, ?, ?)",
		"cached-proj", "## Cached Story", now, 14,
	)
	if err != nil {
		t.Fatalf("insert cache: %v", err)
	}

	layer := NewEssentialLayer(accessor, nil)
	result := layer.Render("cached-proj")

	if result != "## Cached Story" {
		t.Errorf("expected cached story, got %q", result)
	}
}

func TestEssentialLayer_Render_CacheExpired(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	expired := time.Now().UTC().Add(-7 * time.Hour)
	_, err := db.Exec(
		"INSERT INTO essential_stories (project_id, story, generated_at, bytes) VALUES (?, ?, ?, ?)",
		"expired-proj", "## Old Story", expired, 12,
	)
	if err != nil {
		t.Fatalf("insert expired cache: %v", err)
	}

	insertMemory(t, db, "expired-proj", "fact", "Fresh fact", 1)

	layer := NewEssentialLayer(accessor, nil)
	result := layer.Render("expired-proj")

	if result == "## Old Story" {
		t.Error("should have regenerated, not used expired cache")
	}
	if result == "" {
		t.Error("expected non-empty regenerated result")
	}
}

func TestEssentialLayer_Render_CachesResult(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	insertMemory(t, db, "persist-proj", "fact", "Some fact", 1)

	layer := NewEssentialLayer(accessor, nil)
	_ = layer.Render("persist-proj")

	var story string
	err := db.QueryRow("SELECT story FROM essential_stories WHERE project_id = ?", "persist-proj").Scan(&story)
	if err != nil {
		t.Fatalf("cache not saved: %v", err)
	}
	if story == "" {
		t.Error("expected non-empty cached story")
	}
}

func TestEssentialLayer_Invalidate(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	_, err := db.Exec(
		"INSERT INTO essential_stories (project_id, story, generated_at, bytes) VALUES (?, ?, ?, ?)",
		"inv-proj", "## To Invalidate", time.Now().UTC(), 17,
	)
	if err != nil {
		t.Fatalf("insert cache: %v", err)
	}

	layer := NewEssentialLayer(accessor, nil)
	layer.Invalidate("inv-proj")

	var story string
	err = db.QueryRow("SELECT story FROM essential_stories WHERE project_id = ?", "inv-proj").Scan(&story)
	if err != sql.ErrNoRows {
		t.Errorf("expected cache deleted, got %q (err=%v)", story, err)
	}
}

func TestEssentialLayer_Invalidate_EmptyProjectID(t *testing.T) {
	accessor := setupTestDB(t)
	layer := NewEssentialLayer(accessor, nil)
	layer.Invalidate("")
}

func TestEssentialLayer_MergeAndLimit(t *testing.T) {
	a := []essentialMemory{
		{AccessCount: 10, Content: "a1"},
		{AccessCount: 5, Content: "a2"},
	}
	b := []essentialMemory{
		{AccessCount: 8, Content: "b1"},
		{AccessCount: 3, Content: "b2"},
	}

	result := mergeAndLimit(a, b, 3)
	if len(result) != 3 {
		t.Fatalf("expected 3, got %d", len(result))
	}
	if result[0].Content != "a1" {
		t.Errorf("expected a1 first, got %s", result[0].Content)
	}
	if result[1].Content != "b1" {
		t.Errorf("expected b1 second, got %s", result[1].Content)
	}
	if result[2].Content != "a2" {
		t.Errorf("expected a2 third, got %s", result[2].Content)
	}
}

func TestProjectLayer_WithEssential(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	insertMemory(t, db, "p1", "fact", "Test fact", 5)

	layer := NewProjectLayerWithEssential(accessor, "p1", nil)
	result := layer.Render()

	if result == "" {
		t.Fatal("expected non-empty from ProjectLayer with essential")
	}
	if !contains(result, "Test fact") {
		t.Errorf("missing Test fact in:\n%s", result)
	}
}

func TestProjectLayer_WithEssential_Invalidate(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()

	insertMemory(t, db, "p1", "fact", "Test fact", 5)

	layer := NewProjectLayerWithEssential(accessor, "p1", nil)
	_ = layer.Render()

	layer.Invalidate()

	var story string
	err := db.QueryRow("SELECT story FROM essential_stories WHERE project_id = ?", "p1").Scan(&story)
	if err != sql.ErrNoRows {
		t.Errorf("expected cache deleted after invalidate, got %q", story)
	}
}

func TestProjectLayer_LegacyFallback(t *testing.T) {
	layer := NewProjectLayer(func() string { return "legacy story" })
	result := layer.Render()
	if result != "legacy story" {
		t.Errorf("expected legacy story, got %q", result)
	}
}

func TestProjectLayer_NilFallback(t *testing.T) {
	layer := NewProjectLayer(nil)
	result := layer.Render()
	if result != "" {
		t.Errorf("expected empty, got %q", result)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
