package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
)

// TestKGAddDoesNotTakeTheRawCwdAsTheProjectRoot is an exploit test: kg_add
// filters with the cwd it was given as the project root, so outside a
// repository (the linked-project fallback) a cwd above the paths in a triple
// makes them count as inside the project. With cwd "/" a home path outside
// any project goes to the remote with its slashes turned into dots. It fails
// while kg_add passes the raw cwd instead of the resolved repository root.
func TestKGAddDoesNotTakeTheRawCwdAsTheProjectRoot(t *testing.T) {
	_, svc, cfg := newRemoteSearchFixture(t)
	sent := make(chan string, 4)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/triples") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Triples []struct {
				Object string `json:"object"`
			} `json:"triples"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode triples: %v", err)
		}
		for _, tr := range req.Triples {
			sent <- tr.Object
		}
		if _, err := w.Write([]byte(`{"accepted":1}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true, Projects: []string{"rp-linked"}},
	}
	srv := NewServer(svc, kg.New(svc.StoreDB(), slog.Default()), nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{"subject": "backup", "predicate": "reads", "object": "/home/someone/.ssh/id_ed25519", "cwd": "/"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := srv.toolKGAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "blocked by safety policy") {
		t.Errorf("kg_add reply = %q, want a home path outside any project kept local", out)
	}
	select {
	case got := <-sent:
		t.Fatalf("the remote received %q, a home path outside any project", got)
	case <-time.After(300 * time.Millisecond):
	}
}
