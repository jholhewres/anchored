//go:build !windows

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	ctxpkg "github.com/jholhewres/anchored/pkg/context"
	_ "github.com/mattn/go-sqlite3"
)

func newTestServerWithOptimizer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(ctxpkg.MigrationSQL); err != nil {
		t.Fatalf("migration: %v", err)
	}

	cfg := config.ContextOptimizerConfig{
		Enabled:        true,
		DefaultTTL:     336,
		LRUCapMB:       50,
		SandboxTimeout: 30,
		MaxOutputKB:    1024,
		FetchCacheTTL:  24,
	}

	facade, err := NewCtxOptimizer(db, cfg, nil)
	if err != nil {
		t.Fatalf("new ctx optimizer: %v", err)
	}
	t.Cleanup(func() { facade.Close() })

	return &Server{optimizer: facade}
}

func callToolJSON(t *testing.T, s *Server, toolName string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.callTool(context.Background(), toolName, raw)
	if err != nil {
		t.Fatalf("callTool %s: %v", toolName, err)
	}
	return result
}

func TestCtxTools_OptimizerNil(t *testing.T) {
	s := &Server{optimizer: nil}
	for _, name := range []string{
"anchored_execute", "anchored_execute_file", "anchored_batch_execute",
	"anchored_index", "anchored_ctx_search", "anchored_fetch_and_index",
	} {
		result, err := s.callTool(context.Background(), name, json.RawMessage(`{}`))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
		if !strings.Contains(result, "not enabled") {
			t.Errorf("%s: expected 'not enabled' message, got: %s", name, result)
		}
	}
}

func TestCtxExecute_Echo(t *testing.T) {
	s := newTestServerWithOptimizer(t)
	result := callToolJSON(t, s, "anchored_execute", map[string]any{
		"language": "shell",
		"code":     "echo hello_test_world",
	})
	if !strings.Contains(result, "hello_test_world") {
		t.Errorf("expected output to contain 'hello_test_world', got: %s", result)
	}
	if !strings.Contains(result, "Exit: 0") {
		t.Errorf("expected exit 0, got: %s", result)
	}
}

func TestCtxExecute_Timeout(t *testing.T) {
	s := newTestServerWithOptimizer(t)
	result := callToolJSON(t, s, "anchored_execute", map[string]any{
		"language": "shell",
		"code":     "sleep 60",
		"timeout":  1000,
	})
	if !strings.Contains(result, "TIMEOUT") {
		t.Errorf("expected timeout, got: %s", result)
	}
}

func TestCtxIndexAndSearch(t *testing.T) {
	s := newTestServerWithOptimizer(t)

	idxResult := callToolJSON(t, s, "anchored_index", map[string]any{
		"content": "# Test Heading\n\nThe quick brown fox jumps over the lazy dog. UniqueTokenAlphaBetaGamma.",
		"source":  "test-doc",
	})
	if !strings.Contains(idxResult, "Indexed content") {
		t.Errorf("expected indexing confirmation, got: %s", idxResult)
	}

	searchResult := callToolJSON(t, s, "anchored_ctx_search", map[string]any{
		"queries": []string{"UniqueTokenAlphaBetaGamma"},
	})
	if !strings.Contains(searchResult, "test-doc") {
		t.Errorf("expected search to find source 'test-doc', got: %s", searchResult)
	}
}

func TestCtxSearch_NoResults(t *testing.T) {
	s := newTestServerWithOptimizer(t)
	result := callToolJSON(t, s, "anchored_ctx_search", map[string]any{
		"queries": []string{"NonExistentTokenZzzzzz"},
	})
	if !strings.Contains(result, "no results") && !strings.Contains(result, "No results") {
		t.Errorf("expected no results message, got: %s", result)
	}
}
