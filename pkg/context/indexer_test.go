package ctx

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newIndexerTestDB(t *testing.T) *sql.DB {
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

func newTestIndexer(t *testing.T) (*Indexer, *sql.DB) {
	t.Helper()
	db := newIndexerTestDB(t)
	store := NewStore(db, nil)
	if err := store.PrepareStatements(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	chunker := NewChunker(4096)
	indexer := NewIndexer(store, chunker, db, 336, nil)
	return indexer, db
}

func TestIndexer_MarkdownContent(t *testing.T) {
	ix, _ := newTestIndexer(t)
	ctx := context.Background()

	md := `# Setup Guide

Install the dependencies with npm install.

## Configuration

Edit the config.yaml file to match your environment.

### Advanced

Use environment variables for overrides.
`
	sourceID, err := ix.IndexContent(ctx, md, "index", "docs", "sess-1", "prose")
	if err != nil {
		t.Fatalf("IndexContent: %v", err)
	}
	if sourceID == "" {
		t.Fatal("expected non-empty sourceGroupID")
	}

	chunks, err := ix.store.GetChunksBySource(ctx, "index")
	if err != nil {
		t.Fatalf("GetChunksBySource: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks for markdown with headings, got %d", len(chunks))
	}

	foundHeadings := map[string]bool{}
	for _, c := range chunks {
		if c.Label != "" {
			foundHeadings[c.Label] = true
		}
		if c.ContentType != "prose" {
			t.Errorf("ContentType: got %q, want %q", c.ContentType, "prose")
		}
		if c.SessionID != "sess-1" {
			t.Errorf("SessionID: got %q, want %q", c.SessionID, "sess-1")
		}
	}

	if len(foundHeadings) == 0 {
		t.Error("expected at least one chunk with a heading label")
	}
}

func TestIndexer_RawContent(t *testing.T) {
	ix, _ := newTestIndexer(t)
	ctx := context.Background()

	output := strings.Repeat("line of shell output\n", 50)

	sourceID, err := ix.IndexRaw(ctx, output, "execute", "build", "sess-2")
	if err != nil {
		t.Fatalf("IndexRaw: %v", err)
	}
	if sourceID == "" {
		t.Fatal("expected non-empty sourceGroupID")
	}

	chunks, err := ix.store.GetChunksBySource(ctx, "execute")
	if err != nil {
		t.Fatalf("GetChunksBySource: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk for raw content")
	}

	for _, c := range chunks {
		if c.ContentType != "code" {
			t.Errorf("ContentType: got %q, want %q", c.ContentType, "code")
		}
		if c.Source != "execute" {
			t.Errorf("Source: got %q, want %q", c.Source, "execute")
		}
		if c.Label != "build" {
			t.Errorf("Label: got %q, want %q", c.Label, "build")
		}
	}
}

func TestIndexer_DedupDetection(t *testing.T) {
	ix, _ := newTestIndexer(t)
	ctx := context.Background()

	content := "unique content that should be deduplicated when indexed twice"

	id1, err := ix.IndexContent(ctx, content, "index", "first", "sess-1", "prose")
	if err != nil {
		t.Fatalf("first IndexContent: %v", err)
	}
	if id1 == "" {
		t.Fatal("first call should return a sourceGroupID")
	}

	id2, err := ix.IndexContent(ctx, content, "index", "second", "sess-1", "prose")
	if err != nil {
		t.Fatalf("second IndexContent: %v", err)
	}
	if id2 == "" {
		t.Fatal("second call should return a sourceGroupID")
	}
	if id1 == id2 {
		t.Error("two indexing operations should have different sourceGroupIDs")
	}

	chunks, err := ix.store.GetChunksBySource(ctx, "index")
	if err != nil {
		t.Fatalf("GetChunksBySource: %v", err)
	}
	if len(chunks) != 1 {
		t.Errorf("expected exactly 1 chunk after dedup, got %d", len(chunks))
	}
}

func TestIndexer_EmptyContent(t *testing.T) {
	ix, _ := newTestIndexer(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		method  string
		content string
	}{
		{"empty IndexContent", "content", ""},
		{"empty IndexRaw", "raw", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var id string
			var err error

			switch tt.method {
			case "content":
				id, err = ix.IndexContent(ctx, tt.content, "index", "test", "sess-1", "prose")
			case "raw":
				id, err = ix.IndexRaw(ctx, tt.content, "execute", "test", "sess-1")
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != "" {
				t.Errorf("expected empty sourceGroupID for empty content, got %q", id)
			}
		})
	}
}

func TestIndexer_SourceGroupID(t *testing.T) {
	ix, db := newTestIndexer(t)
	ctx := context.Background()

	md := `# Section A

Content for section A.

## Subsection

More details here.

# Section B

Content for section B.
`
	sourceID, err := ix.IndexContent(ctx, md, "index", "docs", "sess-1", "prose")
	if err != nil {
		t.Fatalf("IndexContent: %v", err)
	}
	if sourceID == "" {
		t.Fatal("expected non-empty sourceGroupID")
	}

	rows, err := db.QueryContext(ctx,
		`SELECT metadata FROM content_chunks WHERE metadata LIKE '%' || ? || '%'`,
		sourceID,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		count++
		var meta string
		if err := rows.Scan(&meta); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m indexerMetadata
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if m.SourceGroupID != sourceID {
			t.Errorf("SourceGroupID: got %q, want %q", m.SourceGroupID, sourceID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 chunks with same sourceGroupID, got %d", count)
	}
}
