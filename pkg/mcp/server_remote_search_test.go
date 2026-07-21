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
	"sync/atomic"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

// TestToolSearch_MergesRemoteHits locks the day-to-day contract: when the
// cwd's project has a remote configured, anchored_search transparently merges
// the team server's hits into the local results — no `remote` param needed.
// Remote-origin hits carry origin="remote" so the agent can attribute them.
func TestToolSearch_MergesRemoteHits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")

	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/merge-fixture.git"},
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

	ctx := context.Background()
	if _, err := svc.Save(ctx, "local decision: keep the merge fixture deterministic", "decision", "test", repo); err != nil {
		t.Fatalf("save: %v", err)
	}
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}

	var searchCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"rp-1","name":"merge-fixture","slug":"merge-fixture","remote_key":%q}]`, proj.RemoteKey)
	})
	mux.HandleFunc("/v1/memories/search", func(w http.ResponseWriter, r *http.Request) {
		searchCalls.Add(1)
		if got := r.URL.Query().Get("project_id"); got != "rp-1" {
			t.Errorf("remote search project_id = %q, want rp-1 (the REMOTE id, never the local one)", got)
		}
		if r.URL.Query().Get("q") == "anything" {
			fmt.Fprint(w, `[{"id":"remote-1","category":"decision","content":"remote-only team memory","project_id":"rp-1"}]`)
			return
		}
		if got := r.URL.Query().Get("limit"); got != "30" {
			t.Errorf("remote search limit = %q, want 30 (max(3K, 20) candidates)", got)
		}
		fmt.Fprint(w, `[
			{"id":"remote-copy","category":"decision","content":"local decision: keep the merge fixture deterministic","project_id":"rp-1"},
			{"id":"remote-1","category":"decision","content":"remote-only team memory","project_id":"rp-1"}
		]`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Mirror what config.Load's migrateRemotes() produces for a singular
	// `remote:` block — ResolveRemote only consults the Remotes map.
	cfg.Remotes = map[string]config.RemoteEntry{
		"default": {Name: "default", ServerURL: ts.URL, APIKey: "test-key", Default: true},
	}

	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{"query": "merge fixture", "cwd": repo, "debug": true})
	out, err := srv.toolSearch(ctx, args)
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}

	if searchCalls.Load() == 0 {
		t.Fatal("remote search endpoint was never called — auto-merge did not engage")
	}
	for _, want := range []string{
		"local decision: keep the merge fixture deterministic", // local hit kept
		`origin="local,remote"`,                                // synced duplicate provenance merged
		`score="0.032787"`,                                     // exact 2/(60+rank 1) fused score
		`origins="local,remote"`,                               // debug provenance
		`rrf_ranks="local:1,remote:1"`,                         // debug source ranks
		`origin="remote"`,                                      // remote hit tagged
		"remote-only team memory",                              // remote content present
		`id="remote-1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if got := strings.Count(out, "local decision: keep the merge fixture deterministic"); got != 1 {
		t.Errorf("synced duplicate rendered %d times, want once\n--- output ---\n%s", got, out)
	}
	if !strings.Contains(out, `count="2"`) {
		t.Errorf("fused result count should be 2\n--- output ---\n%s", out)
	}

	// Explicit remote param ("" = default) must search the remote EXCLUSIVELY.
	args, _ = json.Marshal(map[string]any{"query": "anything", "cwd": repo, "remote": ""})
	out, err = srv.toolSearch(ctx, args)
	if err != nil {
		t.Fatalf("toolSearch remote-only: %v", err)
	}
	if !strings.Contains(out, "remote-only team memory") || strings.Contains(out, "local decision:") {
		t.Errorf("remote:\"\" should be remote-exclusive\n--- output ---\n%s", out)
	}
}

// newRemoteSearchFixture builds the git repo + local service shared by the
// explicit-remote tests.
func newRemoteSearchFixture(t *testing.T) (repo string, svc *memory.Service, cfg *config.Config) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo = filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/fallback-fixture.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	cfg = config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "test.db")
	cfg.Embedding.Provider = "none"
	var err error
	svc, err = memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Close)
	if _, err := svc.Save(context.Background(), "local decision: fallback fixture memory", "decision", "test", repo); err != nil {
		t.Fatalf("save: %v", err)
	}
	return repo, svc, cfg
}

// TestToolSearch_RemoteFailureIsVisible locks the anti-hallucination contract:
// when an EXPLICIT remote search cannot reach the server (or can't resolve the
// project there), the local results must be clearly marked with remote_error +
// fallback="local" — never presented as remote data.
func TestToolSearch_RemoteFailureIsVisible(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	dead.Close() // unreachable from the first request

	cfg.Remotes = map[string]config.RemoteEntry{
		"default": {Name: "default", ServerURL: dead.URL, APIKey: "k", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{"query": "fallback fixture", "cwd": repo, "remote": ""})
	out, err := srv.toolSearch(context.Background(), args)
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	for _, want := range []string{"remote_error=", `fallback="local"`, "local decision: fallback fixture memory"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestToolSearch_SemanticCapabilityFallbackStillFederates(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}

	var modes []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w,
			`[{"id":"remote-project","name":"fallback-fixture","slug":"fallback-fixture","remote_key":%q}]`,
			proj.RemoteKey,
		)
	})
	mux.HandleFunc("/v1/memories/search", func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		modes = append(modes, mode)
		if mode == "semantic" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"error":{"code":"semantic_unavailable"}}`)
			return
		}
		w.Header().Set("X-Anchored-Effective-Mode", "text")
		fmt.Fprint(w,
			`[{"id":"remote-fallback","category":"decision","content":"remote lexical fallback memory","project_id":"remote-project","rank":1}]`,
		)
	})
	remote := httptest.NewServer(mux)
	defer remote.Close()

	cfg.Remotes = map[string]config.RemoteEntry{
		"default": {Name: "default", ServerURL: remote.URL, APIKey: "k", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"query": "fallback fixture",
		"cwd":   repo,
		"debug": true,
	})
	out, err := srv.toolSearch(context.Background(), args)
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	if fmt.Sprint(modes) != "[semantic text]" {
		t.Fatalf("search modes=%v, want exactly semantic then text", modes)
	}
	for _, want := range []string{
		"local decision: fallback fixture memory",
		"remote lexical fallback memory",
		`origin="remote"`,
		`requested_mode="semantic"`,
		`effective_mode="text"`,
		`fallback="text"`,
		`fallback_reason="semantic_unavailable"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q\n--- output ---\n%s", want, out)
		}
	}

	modes = nil
	explicitArgs, _ := json.Marshal(map[string]any{
		"query": "fallback fixture", "cwd": repo, "remote": "default",
	})
	explicit, err := srv.toolSearch(context.Background(), explicitArgs)
	if err != nil {
		t.Fatalf("explicit toolSearch: %v", err)
	}
	if fmt.Sprint(modes) != "[semantic text]" {
		t.Fatalf("explicit search modes=%v, want exactly semantic then text", modes)
	}
	for _, want := range []string{
		"remote lexical fallback memory",
		`requested_mode="semantic"`,
		`effective_mode="text"`,
		`fallback="text"`,
		`fallback_reason="semantic_unavailable"`,
	} {
		if !strings.Contains(explicit, want) {
			t.Fatalf("explicit output missing %q\n--- output ---\n%s", want, explicit)
		}
	}
}

// TestToolSearch_DefaultSelectorFollowsOriginRouting locks the selector fix:
// remote:""/"default" means THIS REPO'S remote (origin-probe routing, same as
// sync) — not the config entry that happens to be named "default". The repo's
// project lives on the "team" server; "default" knows nothing about it.
func TestToolSearch_DefaultSelectorFollowsOriginRouting(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}

	// "default" server: does NOT know this repo.
	defaultMux := http.NewServeMux()
	defaultMux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	defaultSrv := httptest.NewServer(defaultMux)
	defer defaultSrv.Close()

	// "team" server: knows the repo's remote_key and has the memory.
	teamMux := http.NewServeMux()
	teamMux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"team-proj","name":"fallback-fixture","slug":"fallback-fixture","remote_key":%q}]`, proj.RemoteKey)
	})
	teamMux.HandleFunc("/v1/memories/search", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("project_id"); got != "team-proj" {
			t.Errorf("search project_id = %q, want team-proj", got)
		}
		fmt.Fprint(w, `[{"id":"team-1","category":"decision","content":"team-routed memory","project_id":"team-proj"}]`)
	})
	teamSrv := httptest.NewServer(teamMux)
	defer teamSrv.Close()

	cfg.Remotes = map[string]config.RemoteEntry{
		"default": {Name: "default", ServerURL: defaultSrv.URL, APIKey: "k", Default: true},
		"team":    {Name: "team", ServerURL: teamSrv.URL, APIKey: "k"},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{"query": "anything", "cwd": repo, "remote": "default"})
	out, err := srv.toolSearch(context.Background(), args)
	if err != nil {
		t.Fatalf("toolSearch: %v", err)
	}
	if !strings.Contains(out, "team-routed memory") || !strings.Contains(out, `remote="team"`) {
		t.Errorf("selector \"default\" must follow origin routing to the team server\n--- output ---\n%s", out)
	}
}
