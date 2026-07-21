package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// TestToolSave_QueuesAutoSyncForResolvedDestination locks both sides of the
// local-first contract: the tool returns after durably queueing the write, and
// the background delivery still resolves the exact server-side project.
func TestToolSave_QueuesAutoSyncForResolvedDestination(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/dest-fixture.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "test.db")
	cfg.Embedding.Provider = "none"

	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}

	delivered := make(chan remotesync.RemoteMemory, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"rp-7","name":"dest-fixture","slug":"dest-fixture","remote_key":%q}]`, proj.RemoteKey)
	})
	mux.HandleFunc("/v1/memories", func(w http.ResponseWriter, r *http.Request) {
		var payload remotesync.RemoteMemory
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		delivered <- payload
		w.Write([]byte(`{"id":"m-1","category":"decision","project_id":"rp-7","created":true}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	cfg.Remotes = map[string]config.RemoteEntry{
		"teamsrv": {Name: "teamsrv", ServerURL: ts.URL, APIKey: "test-key", Default: true},
	}

	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"content":  "decision: destination labeling is part of the sync contract",
		"category": "decision",
		"cwd":      repo,
	})
	out, err := srv.toolSave(context.Background(), args)
	if err != nil {
		t.Fatalf("toolSave: %v", err)
	}

	if !strings.Contains(out, "(auto-sync queued)") {
		t.Fatalf("save result must report durable queueing\n--- output ---\n%s", out)
	}
	select {
	case payload := <-delivered:
		if payload.ProjectID != "rp-7" {
			t.Fatalf("delivered project = %q, want rp-7", payload.ProjectID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued auto-sync was not delivered to the resolved destination")
	}
}
