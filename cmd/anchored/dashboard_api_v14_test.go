package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newV14TestAPI(t *testing.T) (*dashboardAPI, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := ensureRecallSchema(db); err != nil {
		t.Fatalf("ensureRecallSchema: %v", err)
	}
	return &dashboardAPI{db: db, logger: slog.Default()}, db
}

func TestHandleTokens(t *testing.T) {
	api, db := newV14TestAPI(t)
	recordRecall(db, "p1", "sessionstart", 1000, 8000)
	recordRecall(db, "p1", "userpromptsubmit", 500, 8000)

	req := httptest.NewRequest(http.MethodGet, "/api/tokens", nil)
	rec := httptest.NewRecorder()
	api.handleTokens(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if got["injections"].(float64) != 2 {
		t.Errorf("injections = %v, want 2", got["injections"])
	}
	if got["injected_tokens"].(float64) != 1500 {
		t.Errorf("injected_tokens = %v, want 1500", got["injected_tokens"])
	}
	if got["baseline_tokens"].(float64) != 16000 {
		t.Errorf("baseline_tokens = %v, want 16000", got["baseline_tokens"])
	}
}

func TestHandleConnections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Make openclaw "installed" and register anchored in its config.
	ocDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(`{"mcpServers":{"anchored":{"command":"/abs/anchored"}}}`), 0644); err != nil {
		t.Fatal(err)
	}

	api, _ := newV14TestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	rec := httptest.NewRecorder()
	api.handleConnections(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Hosts []hostConnection `json:"hosts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	var oc *hostConnection
	for i := range got.Hosts {
		if got.Hosts[i].Name == "openclaw" {
			oc = &got.Hosts[i]
		}
	}
	if oc == nil {
		t.Fatal("openclaw not in connections list")
	}
	if !oc.Installed || !oc.Registered {
		t.Fatalf("openclaw should be installed+registered, got %+v", *oc)
	}
	// A host with no config present should read as not installed / not registered.
	for _, h := range got.Hosts {
		if h.Name == "hermes" && (h.Installed || h.Registered) {
			t.Errorf("hermes should be absent, got %+v", h)
		}
	}
}
