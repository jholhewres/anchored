package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

// newDashboardTestSvc builds a real Service against a throwaway on-disk SQLite
// DB with embeddings disabled (provider "none") so the test stays offline and
// fast — no ONNX runtime/model download.
func newDashboardTestSvc(t *testing.T) *memory.Service {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Memory:    config.MemoryConfig{DatabasePath: filepath.Join(dir, "t.db"), StorageDir: dir},
		Embedding: config.EmbeddingConfig{Provider: "none"},
	}
	if err := config.EnsureDirs(cfg); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(svc.Close)
	return svc
}

func doJSON(t *testing.T, srv *httptest.Server, method, path string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func asMap(v any) map[string]any { return v.(map[string]any) }
func num(v any) int              { return int(v.(float64)) }

// TestDashboardAPI_Core covers the svc-backed handlers (stats/list/search/get)
// and the soft-delete write path.
func TestDashboardAPI_Core(t *testing.T) {
	svc := newDashboardTestSvc(t)
	ctx := context.Background()
	if _, err := svc.Save(ctx, "we settled on postgres for durable storage", "decision", "test", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "redis is in-memory and very fast", "fact", "test", ""); err != nil {
		t.Fatal(err)
	}

	api := &dashboardAPI{svc: svc, db: svc.StoreDB(), logger: slog.Default()}
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	var stats map[string]any
	if c := doJSON(t, srv, "GET", "/api/stats", &stats); c != http.StatusOK {
		t.Fatalf("stats: %d", c)
	}
	if num(stats["total_memories"]) != 2 {
		t.Errorf("stats total = %v, want 2", stats["total_memories"])
	}

	var list map[string]any
	if c := doJSON(t, srv, "GET", "/api/memories?limit=10", &list); c != http.StatusOK {
		t.Fatalf("memories: %d", c)
	}
	items := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("list items = %d, want 2", len(items))
	}

	// Search handler returns 200 with an items array. (With embeddings
	// disabled the Service falls back to BM25-only expansion, whose recall
	// varies; the handler contract is what we assert here — the hybrid path
	// is exercised live against the real DB during manual validation.)
	var sr map[string]any
	if c := doJSON(t, srv, "GET", "/api/search?q=postgres&limit=5", &sr); c != http.StatusOK {
		t.Fatalf("search: %d", c)
	}
	if _, ok := sr["items"].([]any); !ok {
		t.Errorf("search response missing items array: %v", sr)
	}

	// get one
	id := asMap(items[0])["id"].(string)
	if c := doJSON(t, srv, "GET", "/api/memories/"+id, nil); c != http.StatusOK {
		t.Fatalf("get: %d", c)
	}

	// soft-delete it
	if c := doJSON(t, srv, "DELETE", "/api/memories/"+id, nil); c != http.StatusNoContent {
		t.Fatalf("delete: %d", c)
	}
	var deletedAt any
	_ = svc.StoreDB().QueryRowContext(ctx, "SELECT deleted_at FROM memories WHERE id = ?", id).Scan(&deletedAt)
	if deletedAt == nil {
		t.Error("deleted_at not set after delete")
	}
	// deleting again -> 404
	if c := doJSON(t, srv, "DELETE", "/api/memories/"+id, nil); c != http.StatusNotFound {
		t.Errorf("re-delete: %d, want 404", c)
	}
}

// TestDashboardAPI_SQLHandlers covers the raw-DB handlers: timeline, kg,
// projects, sessions, health. Data is seeded via SQL directly.
func TestDashboardAPI_SQLHandlers(t *testing.T) {
	svc := newDashboardTestSvc(t)
	db := svc.StoreDB()
	ctx := context.Background()

	// seed a project, a session, and a KG triple
	_, err := db.ExecContext(ctx, `INSERT INTO projects (id, name, path, remote_key) VALUES ('p1','demo','/tmp/demo','github.com/me/demo')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO sessions (id, source, directory, message_count, last_activity_at) VALUES ('s1','claude-code','/tmp/demo',7,'2026-06-01 10:00:00')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO kg_entities (id, name) VALUES ('e1','Postgres'),('e2','storage')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO kg_predicates (id, name) VALUES ('pr1','uses_for')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO kg_triples (id, subject_id, predicate_id, object_id, confidence) VALUES ('t1','e1','pr1','e2',0.9)`)
	if err != nil {
		t.Fatal(err)
	}

	api := &dashboardAPI{svc: svc, db: db, logger: slog.Default()}
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	// timeline (day) — empty is fine, must be 200 not 500 on NULL dates
	if c := doJSON(t, srv, "GET", "/api/timeline?bucket=day", nil); c != http.StatusOK {
		t.Errorf("timeline day: %d", c)
	}
	var tl map[string]any
	if c := doJSON(t, srv, "GET", "/api/timeline?bucket=month", &tl); c != http.StatusOK {
		t.Errorf("timeline month: %d", c)
	}

	var kg map[string]any
	if c := doJSON(t, srv, "GET", "/api/kg?limit=50", &kg); c != http.StatusOK {
		t.Fatalf("kg: %d", c)
	}
	triples := kg["triples"].([]any)
	if len(triples) != 1 {
		t.Errorf("kg triples = %d, want 1", len(triples))
	}

	var projs map[string]any
	if c := doJSON(t, srv, "GET", "/api/projects", &projs); c != http.StatusOK {
		t.Fatalf("projects: %d", c)
	}
	if len(projs["items"].([]any)) != 1 {
		t.Errorf("projects items = %d, want 1", len(projs["items"].([]any)))
	}

	var sess map[string]any
	if c := doJSON(t, srv, "GET", "/api/sessions", &sess); c != http.StatusOK {
		t.Fatalf("sessions: %d", c)
	}
	if num(sess["total"]) != 1 {
		t.Errorf("sessions total = %v, want 1", sess["total"])
	}

	var health map[string]any
	if c := doJSON(t, srv, "GET", "/api/health", &health); c != http.StatusOK {
		t.Fatalf("health: %d", c)
	}
	if health["db_bytes"] == nil {
		t.Error("health missing db_bytes")
	}
}

// TestDashboardAPI_Restore covers the restore write path: soft-delete via the
// API hides the memory from Service.Get, then restore brings it back. It also
// pins the idempotency contract (restoring an already-active memory -> 404).
func TestDashboardAPI_Restore(t *testing.T) {
	svc := newDashboardTestSvc(t)
	ctx := context.Background()
	mem, err := svc.Save(ctx, "decisao: postgres para armazenamento duravel", "decision", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	id := mem.ID

	api := &dashboardAPI{svc: svc, db: svc.StoreDB(), logger: slog.Default()}
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	// soft-delete via the API (Service.SoftForget under the hood)
	if c := doJSON(t, srv, "DELETE", "/api/memories/"+id, nil); c != http.StatusNoContent {
		t.Fatalf("delete: %d, want 204", c)
	}
	if m, _ := svc.Get(ctx, id); m != nil {
		t.Fatal("expected memory hidden from Get after soft-delete")
	}

	// restore via the API (Service.Restore under the hood)
	if c := doJSON(t, srv, "POST", "/api/memories/"+id+"/restore", nil); c != http.StatusNoContent {
		t.Fatalf("restore: %d, want 204", c)
	}
	m, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected memory to be visible again after restore")
	}
	if m.ID != id {
		t.Errorf("restored id = %s, want %s", m.ID, id)
	}

	// restoring a memory that isn't deleted -> 404 (no row matches deleted_at IS NOT NULL)
	if c := doJSON(t, srv, "POST", "/api/memories/"+id+"/restore", nil); c != http.StatusNotFound {
		t.Errorf("re-restore: %d, want 404", c)
	}
}

// TestDashboardGuard covers the local-only security middleware: DNS-rebinding
// defence (non-loopback Host rejected), CSRF defence (cross-origin write
// rejected), and the optional bearer token on writes.
func TestDashboardGuard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrap := func(token string) http.Handler {
		return (&dashboardGuard{writeToken: token}).wrap(inner)
	}
	req := func(method, host, origin string) *http.Request {
		r := httptest.NewRequest(method, "/api/memories/x", nil)
		if host != "" {
			r.Host = host
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	withBearer := func(r *http.Request, tok string) *http.Request {
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}
	code := func(h http.Handler, r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	gTok := wrap("s3cret") // --token configured: every request gated
	gNone := wrap("")      // local default: reads + same-origin writes open

	// DNS-rebinding: non-loopback Host is rejected before auth in both modes.
	if c := code(gTok, req("GET", "evil.example.com", "")); c != http.StatusForbidden {
		t.Errorf("non-loopback Host: got %d, want 403", c)
	}
	if c := code(gNone, req("GET", "evil.example.com", "")); c != http.StatusForbidden {
		t.Errorf("non-loopback Host (no token): got %d, want 403", c)
	}

	// Without a token, loopback reads are open.
	if c := code(gNone, req("GET", "127.0.0.1:17777", "")); c != http.StatusOK {
		t.Errorf("loopback GET (no token): got %d, want 200", c)
	}
	// With a token, reads are gated too — no auth -> 401, bearer -> 200.
	if c := code(gTok, req("GET", "127.0.0.1:17777", "")); c != http.StatusUnauthorized {
		t.Errorf("loopback GET without auth (token set): got %d, want 401", c)
	}
	if c := code(gTok, withBearer(req("GET", "127.0.0.1:17777", ""), "s3cret")); c != http.StatusOK {
		t.Errorf("loopback GET with bearer (token set): got %d, want 200", c)
	}

	// CSRF blocks a cross-origin write even when authenticated.
	if c := code(gTok, withBearer(req("DELETE", "127.0.0.1:17777", "https://evil.example.com"), "s3cret")); c != http.StatusForbidden {
		t.Errorf("cross-origin write (authed): got %d, want 403", c)
	}
	if c := code(gNone, req("DELETE", "127.0.0.1:17777", "https://evil.example.com")); c != http.StatusForbidden {
		t.Errorf("cross-origin write (no token): got %d, want 403", c)
	}
	// Same-origin write passes once authed.
	if c := code(gTok, withBearer(req("DELETE", "127.0.0.1:17777", ""), "s3cret")); c != http.StatusOK {
		t.Errorf("same-origin write (authed): got %d, want 200", c)
	}

	// empty Host (HTTP/1.0 / some clients) is allowed locally without a token.
	emptyReq := httptest.NewRequest("GET", "/api/memories/x", nil)
	emptyReq.Host = ""
	if c := code(gNone, emptyReq); c != http.StatusOK {
		t.Errorf("empty Host GET (no token): got %d, want 200", c)
	}
}
