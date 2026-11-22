package mcp

import (
	"context"
	"encoding/json"
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

// TestRemoteSkillProbeCachesASilentRemote locks the skill probe of tools/list
// and anchored_context against a remote that accepts the connection and never
// answers: its deadline outlasts the read probe, so the miss is cached and
// only the first call waits on that remote.
func TestRemoteSkillProbeCachesASilentRemote(t *testing.T) {
	prev := remoteReadProbeTimeout
	remoteReadProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { remoteReadProbeTimeout = prev })
	if remoteSkillPriorityTimeout() <= remoteReadProbeTimeout {
		t.Fatalf("skill probe deadline %s ends before the read probe's %s", remoteSkillPriorityTimeout(), remoteReadProbeTimeout)
	}

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
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	for i := 0; i < 3; i++ {
		start := time.Now()
		if srv.remoteSkillsAdvertisable(context.Background(), repo) {
			t.Fatalf("probe %d advertised skills behind a silent remote", i)
		}
		if took := time.Since(start); i > 0 && took > remoteReadProbeTimeout/2 {
			t.Fatalf("probe %d waited %s on a remote whose miss is cached", i, took)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the silent remote was asked %d times for three probes, want 1", n)
	}
}
