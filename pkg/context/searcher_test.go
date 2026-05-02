package ctx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
)

type testStore struct {
	db *sql.DB
}

func (ts *testStore) SearchChunks(ctx context.Context, query string, maxResults int, contentType, source string) ([]ContentSearchResult, error) {
	q := `
		SELECT cc.id, cc.label, cc.source, cc.content, bm25(content_chunks_fts) AS score
		FROM content_chunks_fts fts
		JOIN content_chunks cc ON cc.rowid = fts.rowid
		WHERE content_chunks_fts MATCH ?
	`
	args := []any{query}

	var conditions []string
	if contentType != "" {
		conditions = append(conditions, "cc.content_type = ?")
		args = append(args, contentType)
	}
	if source != "" {
		conditions = append(conditions, "cc.source LIKE ?")
		args = append(args, "%"+source+"%")
	}
	if len(conditions) > 0 {
		q += " AND " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY score LIMIT ?"
	args = append(args, maxResults)

	rows, err := ts.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("test store search: %w", err)
	}
	defer rows.Close()

	var results []ContentSearchResult
	for rows.Next() {
		var r ContentSearchResult
		var content string
		if err := rows.Scan(&r.ChunkID, &r.Label, &r.Source, &content, &r.Score); err != nil {
			return nil, err
		}
		r.Snippet = content
		results = append(results, r)
	}
	return results, nil
}

func setupTestStore(t *testing.T) *testStore {
	t.Helper()
	db := openTestDB(t)
	if _, err := db.Exec(MigrationSQL); err != nil {
		t.Fatalf("migration: %v", err)
	}
	return &testStore{db: db}
}

func (ts *testStore) insert(t *testing.T, id, source, label, content, contentType string) {
	t.Helper()
	_, err := ts.db.Exec(
		"INSERT INTO content_chunks (id, source, label, content, content_type) VALUES (?, ?, ?, ?, ?)",
		id, source, label, content, contentType,
	)
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func newTestSearcher(ts *testStore) *Searcher {
	return NewSearcher(ts, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestSearcher_ExactSubstringMatch(t *testing.T) {
	ts := setupTestStore(t)
	ts.insert(t, "c1", "execute", "build output", "building project with go build ./... and got error", "prose")
	ts.insert(t, "c2", "execute", "test output", "running tests with go test -v ./pkg/...", "prose")

	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "go build", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'go build'")
	}
	found := false
	for _, r := range results {
		if strings.Contains(strings.ToLower(r.Snippet), "go build") {
			found = true
		}
	}
	if !found {
		t.Errorf("no result snippet contains 'go build'; results: %+v", results)
	}
}

func TestSearcher_MultiTermRanking(t *testing.T) {
	ts := setupTestStore(t)
	ts.insert(t, "c1", "execute", "test", "docker build command failed with exit code 1", "prose")
	ts.insert(t, "c2", "execute", "test", "the docker system is running and the build was mentioned somewhere far away from docker", "prose")

	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "docker build", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}
	if results[0].ChunkID != "c1" {
		t.Errorf("expected c1 (tighter cluster) ranked first, got %s (score %.4f > %.4f)",
			results[0].ChunkID, results[0].Score, results[1].Score)
	}
}

func TestSearcher_ProximityBoosts(t *testing.T) {
	ts := setupTestStore(t)

	ts.insert(t, "tight", "execute", "test", "foo bar baz hello world quick brown fox", "prose")
	ts.insert(t, "loose", "execute", "test", "foo ...................... bar ...................... baz ...................... hello ...................... world ...................... quick ...................... brown ...................... fox", "prose")

	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "hello world", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].ChunkID != "tight" {
		t.Errorf("expected 'tight' ranked first, got %s", results[0].ChunkID)
	}
}

func TestSearcher_SmartSnippet(t *testing.T) {
	ts := setupTestStore(t)

	longContent := strings.Repeat("padding text for spacing ", 50) + "MATCH_TARGET found here" + strings.Repeat(" more padding after match", 50)
	ts.insert(t, "c1", "execute", "long", longContent, "prose")

	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "MATCH_TARGET", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	snippet := results[0].Snippet
	if !strings.Contains(snippet, "MATCH_TARGET") {
		t.Errorf("snippet should contain 'MATCH_TARGET', got: %s", snippet)
	}
	if len(snippet) > 310 {
		t.Errorf("snippet too long: %d chars (including ellipsis), got: %q", len(snippet), snippet)
	}
	if !strings.HasPrefix(snippet, "...") {
		t.Errorf("expected '...' prefix for trimmed content, got: %q", snippet[:20])
	}
}

func TestSearcher_EmptyIndex(t *testing.T) {
	ts := setupTestStore(t)
	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "nonexistent", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty index, got %d", len(results))
	}
}

func TestSearcher_ContentTypeFilter(t *testing.T) {
	ts := setupTestStore(t)
	ts.insert(t, "c1", "execute", "code", "func main() { fmt.Println('hello') }", "code")
	ts.insert(t, "c2", "execute", "docs", "the main function prints hello world to stdout", "prose")

	s := newTestSearcher(ts)
	results, err := s.Search(context.Background(), "main hello", SearchOpts{MaxResults: 10, ContentType: "code"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if r.ChunkID == "c2" {
			t.Error("prose result c2 should not appear when ContentType='code'")
		}
	}
}

func TestSearcher_SingleTermNoPanic(t *testing.T) {
	ts := setupTestStore(t)
	ts.insert(t, "c1", "execute", "test", "hello world example content here", "prose")

	s := newTestSearcher(ts)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("single-term query panicked: %v", r)
		}
	}()

	results, err := s.Search(context.Background(), "hello", SearchOpts{MaxResults: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Error("expected results for single term 'hello'")
	}
}
