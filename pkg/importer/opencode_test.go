package importer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func createOpenCodeTestDB(t *testing.T, basePath string) string {
	t.Helper()
	dbDir := filepath.Join(basePath, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "opencode.db")

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE project (
		id TEXT PRIMARY KEY,
		name TEXT,
		worktree TEXT,
		vcs TEXT,
		time_created INTEGER,
		time_updated INTEGER
	);
	CREATE TABLE session (
		id TEXT PRIMARY KEY,
		project_id TEXT,
		slug TEXT,
		directory TEXT,
		title TEXT,
		version TEXT,
		time_created INTEGER,
		time_updated INTEGER
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY,
		session_id TEXT,
		data TEXT
	);
	CREATE TABLE part (
		id TEXT PRIMARY KEY,
		message_id TEXT,
		session_id TEXT,
		data TEXT
	);
	CREATE TABLE todo (
		session_id TEXT,
		content TEXT,
		status TEXT,
		priority TEXT,
		position INTEGER,
		time_created INTEGER,
		time_updated INTEGER
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	_, _ = db.Exec(`INSERT INTO project (id, name, worktree) VALUES ('p1', 'anchored', '/home/user/Workspace/anchored')`)
	_, _ = db.Exec(`INSERT INTO project (id, name, worktree) VALUES ('p2', 'other-project', '/home/user/Workspace/other')`)

	_, _ = db.Exec(`INSERT INTO session (id, project_id, directory, title) VALUES ('s1', 'p1', '/home/user/Workspace/anchored', 'Test Session')`)
	_, _ = db.Exec(`INSERT INTO session (id, project_id, directory, title) VALUES ('s2', 'p2', '/home/user/Workspace/other', 'Other Session')`)
	_, _ = db.Exec(`INSERT INTO session (id, project_id, directory, title) VALUES ('s3', 'p1', '', 'No Dir Session')`)

	_, _ = db.Exec(`INSERT INTO message (id, session_id, data) VALUES ('m1', 's1', '{"role":"user"}')`)
	_, _ = db.Exec(`INSERT INTO message (id, session_id, data) VALUES ('m2', 's1', '{"role":"assistant"}')`)
	_, _ = db.Exec(`INSERT INTO message (id, session_id, data) VALUES ('m3', 's2', '{"role":"user"}')`)
	_, _ = db.Exec(`INSERT INTO message (id, session_id, data) VALUES ('m4', 's3', '{"role":"user"}')`)

	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, data) VALUES ('pt1', 'm1', 's1', '{"type":"text","text":"How does the deploy work?"}')`)
	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, data) VALUES ('pt2', 'm2', 's1', '{"type":"text","text":"To deploy, run make build and copy the binary."}')`)
	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, data) VALUES ('pt3', 'm2', 's1', '{"type":"step-start"}')`)
	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, data) VALUES ('pt4', 'm3', 's2', '{"type":"text","text":"Refactor the main module"}')`)
	_, _ = db.Exec(`INSERT INTO part (id, message_id, session_id, data) VALUES ('pt5', 'm4', 's3', '{"type":"text","text":"Session with no directory"}')`)

	_, _ = db.Exec(`INSERT INTO todo (session_id, content, status, priority) VALUES ('s1', 'Implement OpenCode importer', 'pending', 'high')`)
	_, _ = db.Exec(`INSERT INTO todo (session_id, content, status, priority) VALUES ('s1', 'Write tests', 'completed', 'medium')`)

	return dbPath
}

type ocMockStore struct {
	mu      sync.Mutex
	saved   []ocMockSave
	saveErr error
}

type ocMockSave struct {
	content  string
	category string
	source   string
	cwd      string
}

func (m *ocMockStore) SaveRaw(_ context.Context, content, category, source, cwd string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = append(m.saved, ocMockSave{content, category, source, cwd})
	return nil
}

func (m *ocMockStore) SaveRawWithSource(_ context.Context, content, category, source string, _ *string, cwd string) error {
	return m.SaveRaw(nil, content, category, source, cwd)
}

func TestOpenCodeImporter_Detect(t *testing.T) {
	tmpDir := t.TempDir()

	importer := NewOpenCodeImporter(tmpDir, nil)
	if importer.Detect() {
		t.Fatal("should not detect without DB")
	}

	createOpenCodeTestDB(t, tmpDir)

	if !importer.Detect() {
		t.Fatal("should detect with DB present")
	}
}

func TestOpenCodeImporter_Name(t *testing.T) {
	imp := NewOpenCodeImporter("/tmp", nil)
	if imp.Name() != "opencode" {
		t.Fatalf("expected 'opencode', got %q", imp.Name())
	}
}

func TestOpenCodeImporter_Import(t *testing.T) {
	tmpDir := t.TempDir()
	createOpenCodeTestDB(t, tmpDir)

	store := &ocMockStore{}
	imp := NewOpenCodeImporter(tmpDir, func(msg string, args ...any) {
		t.Logf("log: %s %v", msg, args)
	})

	result := imp.Import(context.Background(), store)

	t.Logf("Result: found=%d imported=%d skipped=%d errors=%d",
		result.Found, result.Imported, result.Skipped, result.Errors)

	if result.Source != "opencode" {
		t.Fatalf("expected source 'opencode', got %q", result.Source)
	}
	if result.Errors > 0 {
		t.Fatalf("expected 0 errors, got %d", result.Errors)
	}
	if result.Imported == 0 {
		t.Fatal("expected at least 1 imported item")
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	planCount := 0
	factCount := 0
	otherCount := 0

	for _, s := range store.saved {
		switch s.category {
		case "fact":
			factCount++
		case "plan":
			planCount++
		default:
			otherCount++
		}
	}

	if planCount != 2 {
		t.Errorf("expected 2 plan (todos), got %d", planCount)
	}
	if factCount < 2 {
		t.Errorf("expected at least 2 facts (projects), got %d", factCount)
	}

	expectedTotal := factCount + planCount + otherCount
	if result.Imported != expectedTotal {
		t.Errorf("result.Imported=%d but actual saved=%d", result.Imported, expectedTotal)
	}
}

func TestOpenCodeImporter_ImportProjectMapping(t *testing.T) {
	tmpDir := t.TempDir()
	createOpenCodeTestDB(t, tmpDir)

	store := &ocMockStore{}
	imp := NewOpenCodeImporter(tmpDir, nil)
	imp.Import(context.Background(), store)

	store.mu.Lock()
	defer store.mu.Unlock()

	found := false
	for _, s := range store.saved {
		if s.cwd == "/home/user/Workspace/anchored" && s.source == "opencode" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one memory with correct cwd from project mapping")
	}
}

func TestOpenCodeImporter_ImportSessionNoDir(t *testing.T) {
	tmpDir := t.TempDir()
	createOpenCodeTestDB(t, tmpDir)

	store := &ocMockStore{}
	imp := NewOpenCodeImporter(tmpDir, nil)
	imp.Import(context.Background(), store)

	store.mu.Lock()
	defer store.mu.Unlock()

	for _, s := range store.saved {
		if s.content == "Session with no directory" {
			if s.cwd != "/home/user/Workspace/anchored" {
				t.Errorf("expected cwd from project fallback, got %q", s.cwd)
			}
			return
		}
	}
	t.Error("expected 'Session with no directory' to be imported")
}

func TestOpenCodeImporter_ImportSkipsEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	dbDir := filepath.Join(tmpDir, ".local", "share", "opencode")
	_ = os.MkdirAll(dbDir, 0o755)
	dbPath := filepath.Join(dbDir, "opencode.db")

	db, _ := sql.Open("sqlite3", dbPath)
	db.Exec(`CREATE TABLE project (id TEXT PRIMARY KEY, name TEXT, worktree TEXT)`)
	db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT, slug TEXT, directory TEXT, title TEXT, version TEXT, time_created INTEGER, time_updated INTEGER)`)
	db.Exec(`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT)`)
	db.Exec(`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT)`)
	db.Exec(`CREATE TABLE todo (session_id TEXT, content TEXT, status TEXT, priority TEXT, position INTEGER, time_created INTEGER, time_updated INTEGER)`)
	db.Exec(`INSERT INTO project VALUES ('p1', 'test', '/cwd')`)
	db.Exec(`INSERT INTO session VALUES ('s1', 'p1', '', '/cwd', '', '', 0, 0)`)
	db.Exec(`INSERT INTO message VALUES ('m1', 's1', '{"role":"user"}')`)
	db.Exec(`INSERT INTO part VALUES ('pt1', 'm1', 's1', '{"type":"step-start"}')`)
	db.Exec(`INSERT INTO part VALUES ('pt2', 'm1', 's1', '{"type":"text","text":"  "}')`)
	db.Close()

	store := &ocMockStore{}
	imp := NewOpenCodeImporter(tmpDir, nil)
	result := imp.Import(context.Background(), store)

	if result.Imported != 1 {
		t.Errorf("expected 1 imported (project fact only), got %d", result.Imported)
	}
}

func TestOpenCodeImporter_ImportSaveError(t *testing.T) {
	tmpDir := t.TempDir()
	createOpenCodeTestDB(t, tmpDir)

	store := &ocMockStore{saveErr: fmt.Errorf("db error")}
	imp := NewOpenCodeImporter(tmpDir, func(msg string, args ...any) {})
	result := imp.Import(context.Background(), store)

	if result.Skipped == 0 {
		t.Error("expected skipped items when SaveRaw fails")
	}
}

func TestOpenCodeImporter_ImportNoDB(t *testing.T) {
	imp := NewOpenCodeImporter(t.TempDir(), nil)
	store := &ocMockStore{}
	result := imp.Import(context.Background(), store)

	if result.Errors != 1 {
		t.Errorf("expected 1 error (no DB), got errors=%d", result.Errors)
	}
}
