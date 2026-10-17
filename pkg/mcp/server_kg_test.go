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
