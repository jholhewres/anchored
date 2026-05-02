//go:build !windows

package ctx

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	_ "github.com/mattn/go-sqlite3"
)

func testOptimizerConfig() config.ContextOptimizerConfig {
	return config.ContextOptimizerConfig{
		Enabled:        true,
		DefaultTTL:     336,
		LRUCapMB:       50,
		SandboxTimeout: 30,
		MaxOutputKB:    1024,
		FetchCacheTTL:  24,
	}
}

func newTestOptimizer(t *testing.T) *Optimizer {
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

	o, err := NewOptimizer(db, testOptimizerConfig(), nil)
	if err != nil {
		t.Fatalf("new optimizer: %v", err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

func TestOptimizer_Close(t *testing.T) {
	o := newTestOptimizer(t)
	o.Close()
	o.Close()
}

func TestOptimizer_ExecuteAndSearch(t *testing.T) {
	o := newTestOptimizer(t)
	ctx := context.Background()

	res, err := o.Execute(ctx, `print("hello optimizer world")`, "python", 10)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code %d: %s", res.ExitCode, res.Stderr)
	}

	_, err = o.IndexRaw(ctx, res.Stdout, "execute", "test-output")
	if err != nil {
		t.Fatalf("index raw: %v", err)
	}

	results, err := o.Search(ctx, "optimizer", 5, "", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'optimizer'")
	}
	found := false
	for _, r := range results {
		if r.Source == "execute" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a result with source 'execute'")
	}
}

func TestOptimizer_FetchAndIndex(t *testing.T) {
	o := newTestOptimizer(t)
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><h1>Test Page</h1><p>XylophoneQuantumFusion is a rare phenomenon.</p></body></html>`))
	}))
	defer ts.Close()

	result, err := o.FetchAndIndex(ctx, ts.URL, "test-fetch")
	if err != nil {
		t.Fatalf("fetch and index: %v", err)
	}
	if result.FromCache {
		t.Error("first fetch should not be from cache")
	}
	if result.ContentType == "" {
		t.Error("expected content type")
	}

	results, err := o.Search(ctx, "XylophoneQuantumFusion", 5, "", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected search results after fetch+index, got markdown:\n%s", result.Markdown)
	}
}

func TestOptimizer_BatchExecute(t *testing.T) {
	o := newTestOptimizer(t)
	ctx := context.Background()

	commands := []BatchCommand{
		{Label: "echo hello", Command: `print("batch hello world")`, Language: "python"},
		{Label: "echo foo", Command: `print("batch foo bar")`, Language: "python"},
	}
	queries := []string{"batch hello"}

	result, err := o.ExecuteBatch(ctx, commands, queries, "batch")
	if err != nil {
		t.Fatalf("execute batch: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result.Results))
	}
	for i, r := range result.Results {
		if r.ExitCode != 0 {
			t.Errorf("result[%d] exit code %d: %s", i, r.ExitCode, r.Stderr)
		}
	}
	if result.SourceID == "" {
		t.Error("expected non-empty source ID")
	}
}

func TestOptimizer_IndexContent(t *testing.T) {
	o := newTestOptimizer(t)
	ctx := context.Background()

	md := `# Architecture Overview

The system uses a microservices pattern with event sourcing.

## Database Layer

We use SQLite with FTS5 for full-text search.
`

	sourceID, err := o.IndexContent(ctx, md, "test-docs", "architecture", "prose")
	if err != nil {
		t.Fatalf("index content: %v", err)
	}
	if sourceID == "" {
		t.Error("expected non-empty source ID")
	}

	results, err := o.Search(ctx, "microservices", 5, "prose", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results for 'microservices'")
	}

	results2, err := o.Search(ctx, "FTS5", 5, "", "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected search results for 'FTS5'")
	}
}
