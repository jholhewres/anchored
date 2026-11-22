package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// TestSaveRemote_RunsTheClassifier locks `anchored save --remote` to the
// auto-sync classifier: a memory saved outside a repository (user scope) or
// carrying a secret stays local with a warning that names the reason, and a
// project decision is still sent.
func TestSaveRemote_RunsTheClassifier(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	key := gitRepo(t, repo, "https://github.com/acme/classifier.git")
	scratch := filepath.Join(base, "scratch")

	var mu sync.Mutex
	var sent []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects":
			if _, err := fmt.Fprintf(w, `[{"id":"rp-1","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
				t.Errorf("write projects: %v", err)
			}
		case "/v1/memories":
			var m remotesync.RemoteMemory
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				t.Errorf("decode memory: %v", err)
			}
			mu.Lock()
			sent = append(sent, m.Content)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			if _, err := w.Write([]byte(`{"id":"r1","created":true}`)); err != nil {
				t.Errorf("write: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"default": {ServerURL: remote.URL, APIKey: "<REDACTED>", Default: true, Projects: []string{"rp-1"}},
	})
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		dir, content, category, reason string
	}{
		{scratch, "personal note: the staging cluster restarts every night at three", "fact", "user_scope_blocked"},
		{repo, "the deploy bot authenticates with ghp_" + strings.Repeat("a", 36), "decision", "secret_pattern"},
	} {
		out, code := runCLI(t, tc.dir, "save", tc.content, "--category", tc.category, "--remote=default", "--config", cfgPath)
		if code != 0 || !strings.Contains(out, "blocked by the safety policy ("+tc.reason+")") {
			t.Errorf("save %q exited %d, want a warning naming %s\n%s", tc.content, code, tc.reason, out)
		}
	}
	const decision = "Decided that remote saves named on the command line run the same classifier as auto-sync"
	out, code := runCLI(t, repo, "save", decision, "--category", "decision", "--remote=default", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "Saved to remote default") {
		t.Fatalf("project decision: exited %d, want it sent\n%s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != decision {
		t.Fatalf("the remote received %q, want only the project decision", sent)
	}
}
