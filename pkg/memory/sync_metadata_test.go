package memory

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:?_journal_mode=WAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrate011_SyncMetadata(t *testing.T) {
	db := openTestDB(t)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify sync_state table exists
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='sync_state'").Scan(&name)
	if err != nil {
		t.Fatal("sync_state table not found")
	}

	// Verify sync_dirty column exists on memories
	rows, err := db.Query("PRAGMA table_info(memories)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid int
		var cname, ctype string
		var notnull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatal(err)
		}
		if cname == "sync_dirty" {
			found = true
		}
	}
	if !found {
		t.Fatal("sync_dirty column not found on memories table")
	}

	// Verify migration is recorded
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM migrations WHERE name = '011_sync_metadata'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected 1 migration record, got %d", count)
	}
}

func TestSave_WithSyncFields(t *testing.T) {
	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	author := "test-author"
	rpk := "remote-proj-1"
	m := Memory{
		Content:          "test sync fields",
		Category:         "fact",
		Source:           "test",
		SyncDirty:        true,
		SyncOrigin:       "remote",
		Author:           &author,
		RemoteProjectKey: &rpk,
	}

	ctx := context.Background()
	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.FindByContentHash(ctx, contentHash("test sync fields"), nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil {
		t.Fatal("expected memory, got nil")
	}
	if !got.SyncDirty {
		t.Error("expected SyncDirty=true")
	}
	if got.SyncOrigin != "remote" {
		t.Errorf("expected SyncOrigin=remote, got %s", got.SyncOrigin)
	}
	if got.Author == nil || *got.Author != "test-author" {
		t.Error("expected Author=test-author")
	}
	if got.RemoteProjectKey == nil || *got.RemoteProjectKey != "remote-proj-1" {
		t.Error("expected RemoteProjectKey=remote-proj-1")
	}
}

func TestSave_DefaultSyncFields(t *testing.T) {
	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	m := Memory{
		Content:  "defaults test",
		Category: "fact",
		Source:   "test",
	}

	ctx := context.Background()
	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.FindByContentHash(ctx, contentHash("defaults test"), nil)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil {
		t.Fatal("expected memory, got nil")
	}
	if got.SyncDirty {
		t.Error("expected SyncDirty=false by default")
	}
	// sync_origin defaults to empty string when not explicitly set via Go;
	// the SQL DEFAULT 'local' only applies when the column is omitted from INSERT.
}

func TestList_SourceFilter(t *testing.T) {
	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	store.Save(ctx, Memory{Content: "from claude", Category: "fact", Source: "claude-code"})
	store.Save(ctx, Memory{Content: "from test", Category: "fact", Source: "test"})

	memories, err := store.List(ctx, ListOptions{Source: "claude-code"})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(memories))
	}
	if memories[0].Source != "claude-code" {
		t.Errorf("expected source claude-code, got %s", memories[0].Source)
	}
}

func TestList_IncludeDeleted(t *testing.T) {
	store, err := NewSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	store.Save(ctx, Memory{Content: "alive", Category: "fact", Source: "test"})

	store.Save(ctx, Memory{Content: "to delete", Category: "fact", Source: "test"})
	deleted, err := store.FindByContentHash(ctx, contentHash("to delete"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store.SoftDelete(ctx, deleted.ID)

	memories, err := store.List(ctx, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 {
		t.Fatalf("expected 1 (non-deleted), got %d", len(memories))
	}

	memories, err = store.List(ctx, ListOptions{IncludeDeleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 2 {
		t.Fatalf("expected 2 (all), got %d", len(memories))
	}
}

// mockObserver is a test MemoryObserver
type mockObserver struct {
	mu            sync.Mutex
	savedCalls    []Memory
	updatedCalls  []Memory
	deletedCalls  []deletedCall
	restoredCalls []deletedCall
	panicOnSave   bool
}

type deletedCall struct {
	ID        string
	ProjectID *string
}

func (m *mockObserver) OnMemorySaved(_ context.Context, mem Memory) {
	if m.panicOnSave {
		panic("observer panic!")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savedCalls = append(m.savedCalls, mem)
}

func (m *mockObserver) OnMemoryUpdated(_ context.Context, mem Memory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updatedCalls = append(m.updatedCalls, mem)
}

func (m *mockObserver) OnMemoryDeleted(_ context.Context, id string, projectID *string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletedCalls = append(m.deletedCalls, deletedCall{ID: id, ProjectID: projectID})
}

func (m *mockObserver) OnMemoryRestored(_ context.Context, id string, projectID *string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoredCalls = append(m.restoredCalls, deletedCall{ID: id, ProjectID: projectID})
}

func TestObserver_OnMemorySaved(t *testing.T) {
	svc, err := NewService(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	obs := &mockObserver{}
	svc.RegisterObserver(obs)

	ctx := context.Background()
	_, err = svc.Save(ctx, "observer test content", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	// Observer is called in a goroutine, wait briefly
	time.Sleep(100 * time.Millisecond)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.savedCalls) != 1 {
		t.Fatalf("expected 1 OnMemorySaved call, got %d", len(obs.savedCalls))
	}
	if obs.savedCalls[0].Content != "observer test content" {
		t.Errorf("expected content 'observer test content', got %s", obs.savedCalls[0].Content)
	}
}

func TestObserver_OnMemoryUpdated(t *testing.T) {
	svc, err := NewService(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	obs := &mockObserver{}
	svc.RegisterObserver(obs)

	ctx := context.Background()
	mem, err := svc.Save(ctx, "original content for update", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	// SaveWithOptions doesn't propagate the generated ID back to the caller
	// when it's a new memory (value semantics). Look it up via store.
	if mem.ID == "" {
		found, _ := svc.store.FindByContentHash(ctx, contentHash("original content for update"), nil)
		if found != nil {
			mem.ID = found.ID
		}
	}

	_, err = svc.Update(ctx, mem.ID, "updated content", "fact")
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.updatedCalls) != 1 {
		t.Fatalf("expected 1 OnMemoryUpdated call, got %d", len(obs.updatedCalls))
	}
}

func TestObserver_OnMemoryDeleted(t *testing.T) {
	svc, err := NewService(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	obs := &mockObserver{}
	svc.RegisterObserver(obs)

	ctx := context.Background()
	mem, err := svc.Save(ctx, "to be deleted", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	err = svc.Forget(ctx, mem.ID)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.deletedCalls) != 1 {
		t.Fatalf("expected 1 OnMemoryDeleted call, got %d", len(obs.deletedCalls))
	}
	if obs.deletedCalls[0].ID != mem.ID {
		t.Errorf("expected id %s, got %s", mem.ID, obs.deletedCalls[0].ID)
	}
}

func TestObserver_PanicRecovery(t *testing.T) {
	svc, err := NewService(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	obs := &mockObserver{panicOnSave: true}
	svc.RegisterObserver(obs)

	ctx := context.Background()
	_, err = svc.Save(ctx, "should not fail despite observer panic", "fact", "test", "")
	if err != nil {
		t.Fatalf("save should succeed despite observer panic, got: %v", err)
	}
}

func TestObserver_SoftForget(t *testing.T) {
	svc, err := NewService(testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	obs := &mockObserver{}
	svc.RegisterObserver(obs)

	ctx := context.Background()
	mem, err := svc.Save(ctx, "soft delete me", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	err = svc.SoftForget(ctx, mem.ID)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.deletedCalls) != 1 {
		t.Fatalf("expected 1 OnMemoryDeleted call, got %d", len(obs.deletedCalls))
	}
}

func testConfig() *config.Config {
	return &config.Config{
		Memory:    config.MemoryConfig{DatabasePath: ":memory:"},
		Embedding: config.EmbeddingConfig{ModelDir: "/nonexistent"},
	}
}
