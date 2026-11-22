package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
)

// TestKGAddHoldsTheTripleBehindADownRemote locks kg_add's write rule: while
// the routed remote is not answering, the triple stays local, the reply says
// why, and no remote receives it.
func TestKGAddHoldsTheTripleBehindADownRemote(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	var triples atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/triples") {
			triples.Add(1)
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: down.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, kg.New(svc.StoreDB(), slog.Default()), nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{"subject": "anchored", "predicate": "uses", "object": "sqlite", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	out, err := srv.toolKGAdd(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "local only") || !strings.Contains(out, `remote "team"`) {
		t.Fatalf("kg_add reply = %q, want it held behind team", out)
	}
	time.Sleep(100 * time.Millisecond)
	if n := triples.Load(); n != 0 {
		t.Fatalf("a remote received %d triples while the write was held", n)
	}
}

// TestKGAddHoldsBehindASilentRemoteOnce locks kg_add against a remote that
// accepts the connection and never answers, not one that fails fast: the
// first call waits out its budget and holds the triple, and the hold is kept,
// so the next calls neither wait nor ask that remote again.
func TestKGAddHoldsBehindASilentRemoteOnce(t *testing.T) {
	prev := kgAddWriteBudget
	kgAddWriteBudget = 300 * time.Millisecond
	t.Cleanup(func() { kgAddWriteBudget = prev })

	repo, svc, cfg := newRemoteSearchFixture(t)
	var asked atomic.Int32
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(silent.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: silent.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, kg.New(svc.StoreDB(), slog.Default()), nil, nil, cfg, "test", slog.Default())

	for i, object := range []string{"sqlite", "onnx", "fts5"} {
		args, err := json.Marshal(map[string]any{"subject": "anchored", "predicate": "uses", "object": object, "cwd": repo})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		out, err := srv.toolKGAdd(context.Background(), args)
		took := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "local only") || !strings.Contains(out, `remote "team" did not answer`) {
			t.Fatalf("kg_add %d reply = %q, want it held behind team", i, out)
		}
		if i > 0 && took > kgAddWriteBudget/2 {
			t.Fatalf("kg_add %d waited %s on a remote already holding writes", i, took)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the silent remote was asked %d times for three kg_add calls, want 1", n)
	}
}

// TestKGAddKeepsAnUnsafeTripleLocal locks kg_add's safety filter: a triple
// carrying a credential and a home path stays local, the reply says so, and
// the remote is neither asked about the repository nor sent the triple. A
// clean triple on the same repository still goes out.
func TestKGAddKeepsAnUnsafeTripleLocal(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	var lookups atomic.Int32
	objects := make(chan string, 4)
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
				objects <- tr.Object
			}
			if _, err := w.Write([]byte(`{"accepted":1}`)); err != nil {
				t.Errorf("write: %v", err)
			}
		case r.URL.Path == "/v1/projects":
			lookups.Add(1)
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, kg.New(svc.StoreDB(), slog.Default()), nil, nil, cfg, "test", slog.Default())
	add := func(object string) string {
		t.Helper()
		args, err := json.Marshal(map[string]any{"subject": "staging-db", "predicate": "connects_with", "object": object, "cwd": repo})
		if err != nil {
			t.Fatal(err)
		}
		out, err := srv.toolKGAdd(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := add("postgres://app:not-a-real-pass@10.0.0.5/app via /home/someone/secrets/db.env")
	if !strings.Contains(out, "local only — blocked by safety policy") {
		t.Fatalf("kg_add reply = %q, want it kept local by the safety policy", out)
	}
	if n := lookups.Load(); n != 0 {
		t.Fatalf("the remote was asked about the repository %d times for a blocked triple", n)
	}

	if out := add("postgres"); !strings.Contains(out, "auto-sync → team") {
		t.Fatalf("clean kg_add reply = %q, want it sent to team", out)
	}
	select {
	case got := <-objects:
		if got != "postgres" {
			t.Fatalf("the remote received object %q, want only the clean one", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the clean triple never reached the remote")
	}
	select {
	case got := <-objects:
		t.Fatalf("the remote received a second triple %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}
