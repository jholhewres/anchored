package ctx

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func openStoreTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("migration: %v", err)
	}
	return db
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := openStoreTestDB(t)
	s := NewStore(db, nil)
	if err := s.PrepareStatements(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return s
}

func TestStore_InsertAndGetChunk(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	chunk := &Chunk{
		SessionID:   "sess-1",
		Source:      "execute",
		Label:       "build output",
		Content:     "compilation succeeded with 0 errors",
		Metadata:    `{"exit":0}`,
		ContentType: "code",
		IndexedAt:   now,
		TTLHours:    24,
	}

	if err := s.InsertChunk(ctx, chunk); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if chunk.ID == "" {
		t.Fatal("expected UUID to be assigned")
	}

	got, err := s.GetChunk(ctx, chunk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected chunk, got nil")
	}

	if got.SessionID != "sess-1" {
		t.Errorf("SessionID: got %q, want %q", got.SessionID, "sess-1")
	}
	if got.Source != "execute" {
		t.Errorf("Source: got %q, want %q", got.Source, "execute")
	}
	if got.Label != "build output" {
		t.Errorf("Label: got %q, want %q", got.Label, "build output")
	}
	if got.Content != "compilation succeeded with 0 errors" {
		t.Errorf("Content mismatch")
	}
	if got.Metadata != `{"exit":0}` {
		t.Errorf("Metadata: got %q", got.Metadata)
	}
	if got.ContentType != "code" {
		t.Errorf("ContentType: got %q, want %q", got.ContentType, "code")
	}
	if got.TTLHours != 24 {
		t.Errorf("TTLHours: got %d, want 24", got.TTLHours)
	}
}

func TestStore_SearchChunksTrigram(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	chunks := []*Chunk{
		{SessionID: "s1", Source: "execute", Label: "build", Content: "go build: compilation succeeded", ContentType: "code", IndexedAt: now, TTLHours: 24},
		{SessionID: "s1", Source: "execute", Label: "test", Content: "all tests passed successfully", ContentType: "code", IndexedAt: now, TTLHours: 24},
		{SessionID: "s1", Source: "fetch", Label: "docs", Content: "Go documentation for net/http package", ContentType: "prose", IndexedAt: now, TTLHours: 48},
	}
	for _, c := range chunks {
		if err := s.InsertChunk(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	results, err := s.SearchChunks(ctx, "compilation", 10, "", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Label != "build" {
		t.Errorf("label: got %q, want %q", results[0].Label, "build")
	}
	if results[0].Score <= 0 {
		t.Errorf("score should be positive, got %f", results[0].Score)
	}

	results, err = s.SearchChunks(ctx, "Go documentation", 10, "", "")
	if err != nil {
		t.Fatalf("search multi-term: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for multi-term, got %d", len(results))
	}
	if results[0].Label != "docs" {
		t.Errorf("label: got %q, want %q", results[0].Label, "docs")
	}
}

func TestStore_GetExpiredChunkIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	chunks := []*Chunk{
		{Source: "execute", Content: "expired content", ContentType: "code", IndexedAt: now.Add(-48 * time.Hour), TTLHours: 1},
		{Source: "execute", Content: "still valid", ContentType: "code", IndexedAt: now, TTLHours: 24},
		{Source: "execute", Content: "no ttl", ContentType: "code", IndexedAt: now.Add(-100 * time.Hour), TTLHours: 0},
	}
	for _, c := range chunks {
		if err := s.InsertChunk(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	ids, err := s.GetExpiredChunkIDs(ctx)
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 expired, got %d: %v", len(ids), ids)
	}
	if ids[0] != chunks[0].ID {
		t.Errorf("expired ID mismatch: got %q, want %q", ids[0], chunks[0].ID)
	}
}

func TestStore_GetTotalSize(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	contents := []string{"hello world", "foo bar baz", "x"}
	for i, c := range contents {
		if err := s.InsertChunk(ctx, &Chunk{
			Source: "execute", Content: c, ContentType: "code", IndexedAt: now, TTLHours: 1,
		}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	size, err := s.GetTotalSize(ctx)
	if err != nil {
		t.Fatalf("total size: %v", err)
	}
	expected := int64(len("hello world") + len("foo bar baz") + len("x"))
	if size != expected {
		t.Errorf("size: got %d, want %d", size, expected)
	}
}

func TestStore_DeleteChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	var ids []string
	for _, content := range []string{"aaa", "bbb", "ccc"} {
		c := &Chunk{Source: "execute", Content: content, ContentType: "code", IndexedAt: now, TTLHours: 1}
		if err := s.InsertChunk(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
		ids = append(ids, c.ID)
	}

	if err := s.DeleteChunks(ctx, ids[:2]); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for i, id := range ids {
		got, err := s.GetChunk(ctx, id)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if i < 2 {
			if got != nil {
				t.Errorf("chunk %d should be deleted", i)
			}
		} else if got == nil {
			t.Errorf("chunk %d should still exist", i)
		}
	}
}

func TestStore_InsertAndQueryEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	events := []*SessionEvent{
		{SessionID: "sess-a", EventType: "tool_use", Priority: 2, ToolName: "Bash", Summary: "ran build", CreatedAt: now.Add(-2 * time.Minute)},
		{SessionID: "sess-a", EventType: "tool_use", Priority: 3, ToolName: "Read", Summary: "read file", CreatedAt: now.Add(-1 * time.Minute)},
		{SessionID: "sess-b", EventType: "error", Priority: 1, ToolName: "", Summary: "crash", CreatedAt: now},
	}
	for _, e := range events {
		if err := s.InsertEvent(ctx, e); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}

	got, err := s.QueryEvents(ctx, "sess-a", 10)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events for sess-a, got %d", len(got))
	}
	if got[0].Summary != "read file" {
		t.Errorf("first event should be most recent, got %q", got[0].Summary)
	}
	if got[1].Summary != "ran build" {
		t.Errorf("second event: got %q, want %q", got[1].Summary, "ran build")
	}

	if err := s.DeleteEventsBySession(ctx, "sess-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = s.QueryEvents(ctx, "sess-a", 10)
	if err != nil {
		t.Fatalf("query after delete: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 events after delete, got %d", len(got))
	}

	got, err = s.QueryEvents(ctx, "sess-b", 10)
	if err != nil {
		t.Fatalf("query sess-b: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("sess-b should still have 1 event, got %d", len(got))
	}
}

func TestStore_ContentTypeFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	codeChunk := &Chunk{Source: "execute", Label: "code", Content: "function hello() { return 'world' }", ContentType: "code", IndexedAt: now, TTLHours: 24}
	proseChunk := &Chunk{Source: "fetch", Label: "prose", Content: "documentation for the hello function", ContentType: "prose", IndexedAt: now, TTLHours: 24}
	if err := s.InsertChunk(ctx, codeChunk); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChunk(ctx, proseChunk); err != nil {
		t.Fatal(err)
	}

	codeResults, err := s.SearchChunks(ctx, "hello", 10, "code", "")
	if err != nil {
		t.Fatalf("search code: %v", err)
	}
	for _, r := range codeResults {
		if r.Source != "execute" {
			t.Errorf("expected only code results, got source %q", r.Source)
		}
	}

	proseResults, err := s.SearchChunks(ctx, "hello", 10, "prose", "")
	if err != nil {
		t.Fatalf("search prose: %v", err)
	}
	for _, r := range proseResults {
		if r.Source != "fetch" {
			t.Errorf("expected only prose results, got source %q", r.Source)
		}
	}

	allResults, err := s.SearchChunks(ctx, "hello", 10, "", "")
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	if len(allResults) != 2 {
		t.Errorf("expected 2 unfiltered results, got %d", len(allResults))
	}
}

func TestStore_ConcurrentInserts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c := &Chunk{
				Source:      "execute",
				Label:       fmt.Sprintf("chunk-%d", i),
				Content:     fmt.Sprintf("content for chunk number %d", i),
				ContentType: "code",
				IndexedAt:   now,
				TTLHours:    1,
			}
			if err := s.InsertChunk(ctx, c); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent insert: %v", err)
	}

	size, err := s.GetTotalSize(ctx)
	if err != nil {
		t.Fatalf("total size: %v", err)
	}
	if size == 0 {
		t.Error("expected non-zero total size after concurrent inserts")
	}

	results, err := s.SearchChunks(ctx, "chunk", n, "", "")
	if err != nil {
		t.Fatalf("search after concurrent: %v", err)
	}
	if len(results) != n {
		t.Errorf("expected %d results, got %d", n, len(results))
	}
}
