package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// TestKGAddHonoursTheUsersCustomSecretPatterns is an exploit test: the remote
// safety filter builds its sanitizer with the built-in rules only, and triples
// are never sanitized locally, so a value matching a custom pattern the user
// configured goes to the remote. It fails while the filter drops
// cfg.Sanitizer.Patterns.
func TestKGAddHonoursTheUsersCustomSecretPatterns(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	sent := make(chan string, 4)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/triples"):
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
		case r.URL.Path == "/v1/projects":
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	cfg.Sanitizer.Patterns = []string{`acme_[0-9a-f]{24}`}
	t.Cleanup(func() { remotesync.UseSecretPatterns(nil) })
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, kg.New(svc.StoreDB(), slog.Default()), nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{"subject": "billing", "predicate": "authenticates_with", "object": "acme_0123456789abcdef01234567", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	out, err := srv.toolKGAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "blocked by safety policy") {
		t.Errorf("kg_add reply = %q, want a value matching a custom secret pattern kept local", out)
	}
	select {
	case got := <-sent:
		t.Fatalf("the remote received %q, which matches the user's custom secret pattern", got)
	case <-time.After(300 * time.Millisecond):
	}
}
