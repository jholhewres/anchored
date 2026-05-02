package ctx

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newBatchTestEnv(t *testing.T) (*BatchExecutor, *sql.DB) {
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
	if _, err := db.Exec(MigrationSQL009); err != nil {
		t.Fatalf("migration 009: %v", err)
	}

	store := NewStore(db, nil)
	if err := store.PrepareStatements(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	chunker := NewChunker(4096)
	indexer := NewIndexer(store, chunker, db, 336, nil)
	searcher := NewSearcher(store, nil)
	sandbox := NewSandbox(10*time.Second, 1<<20, "")
	batch := NewBatchExecutor(sandbox, indexer, searcher, nil)

	return batch, db
}

func TestBatchExecutor_SingleCommand(t *testing.T) {
	be, _ := newBatchTestEnv(t)
	ctx := context.Background()

	result, err := be.ExecuteBatch(ctx, []BatchCommand{
		{Label: "echo", Command: "print('hello from batch')", Language: "python"},
	}, nil, "sess-batch-1", "", "")
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", result.Results[0].ExitCode)
	}
	if !strings.Contains(result.Results[0].Stdout, "hello from batch") {
		t.Errorf("Stdout: got %q, want to contain 'hello from batch'", result.Results[0].Stdout)
	}
	if result.SourceID == "" {
		t.Error("expected non-empty SourceID")
	}
	if result.TotalBytes == 0 {
		t.Error("expected TotalBytes > 0")
	}
}

func TestBatchExecutor_MultiCommand(t *testing.T) {
	be, _ := newBatchTestEnv(t)
	ctx := context.Background()

	result, err := be.ExecuteBatch(ctx, []BatchCommand{
		{Label: "echo1", Command: "print('first')", Language: "python"},
		{Label: "echo2", Command: "print('second')", Language: "python"},
		{Label: "echo3", Command: "print('third')", Language: "python"},
	}, nil, "sess-batch-2", "", "")
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if len(result.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result.Results))
	}

	expected := []string{"first", "second", "third"}
	for i, exp := range expected {
		if result.Results[i].ExitCode != 0 {
			t.Errorf("result[%d].ExitCode: got %d, want 0", i, result.Results[i].ExitCode)
		}
		if !strings.Contains(result.Results[i].Stdout, exp) {
			t.Errorf("result[%d].Stdout: got %q, want to contain %q", i, result.Results[i].Stdout, exp)
		}
	}
	if result.SourceID == "" {
		t.Error("expected non-empty SourceID")
	}
}

func TestBatchExecutor_FailedCommandContinues(t *testing.T) {
	be, _ := newBatchTestEnv(t)
	ctx := context.Background()

	result, err := be.ExecuteBatch(ctx, []BatchCommand{
		{Label: "good", Command: "print('before fail')", Language: "python"},
		{Label: "bad", Command: "import sys; sys.exit(42)", Language: "python"},
		{Label: "after", Command: "print('after fail')", Language: "python"},
	}, nil, "sess-batch-3", "", "")
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if len(result.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(result.Results))
	}

	if result.Results[0].ExitCode != 0 {
		t.Errorf("result[0].ExitCode: got %d, want 0", result.Results[0].ExitCode)
	}
	if result.Results[1].ExitCode != 42 {
		t.Errorf("result[1].ExitCode: got %d, want 42", result.Results[1].ExitCode)
	}
	if result.Results[2].ExitCode != 0 {
		t.Errorf("result[2].ExitCode: got %d, want 0", result.Results[2].ExitCode)
	}

	if !strings.Contains(result.Results[0].Stdout, "before fail") {
		t.Errorf("result[0].Stdout should contain 'before fail', got %q", result.Results[0].Stdout)
	}
	if !strings.Contains(result.Results[2].Stdout, "after fail") {
		t.Errorf("result[2].Stdout should contain 'after fail', got %q", result.Results[2].Stdout)
	}
}

func TestBatchExecutor_WithSearch(t *testing.T) {
	be, _ := newBatchTestEnv(t)
	ctx := context.Background()

	cmds := []BatchCommand{
		{Label: "deps", Command: "print('dependency: databaselayer version 2.1')", Language: "python"},
	}
	queries := []string{"databaselayer"}

	result, err := be.ExecuteBatch(ctx, cmds, queries, "sess-batch-4", "", "")
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
	if result.Results[0].ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", result.Results[0].ExitCode)
	}
	if len(result.SearchResults) == 0 {
		t.Error("expected search results, got none")
	}
}

func TestBatchExecutor_EmptyCommands(t *testing.T) {
	be, _ := newBatchTestEnv(t)
	ctx := context.Background()

	result, err := be.ExecuteBatch(ctx, nil, nil, "sess-batch-5", "", "")
	if err != nil {
		t.Fatalf("ExecuteBatch: %v", err)
	}

	if len(result.Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(result.Results))
	}
	if result.SourceID != "" {
		t.Errorf("expected empty SourceID for no commands, got %q", result.SourceID)
	}
	if result.TotalBytes != 0 {
		t.Errorf("expected 0 TotalBytes, got %d", result.TotalBytes)
	}
}
