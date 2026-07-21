package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

func TestToolSaveRemoteOutboxRetriesWithStableIdempotencyKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/outbox-fixture.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "outbox.db")
	cfg.Embedding.Provider = "none"
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil {
		t.Fatalf("resolve project = (%#v, %v)", proj, err)
	}

	var (
		mu      sync.Mutex
		keys    []string
		payload []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":"remote-project","name":"outbox","slug":"outbox","remote_key":%q}]`, proj.RemoteKey)
	})
	mux.HandleFunc("/v1/memories", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		payload = append(payload, string(body))
		attempt := len(keys)
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"remote-memory","category":"decision","project_id":"remote-project","created":true}`))
	})
	remote := httptest.NewServer(mux)
	defer remote.Close()
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {
			Name: "team", ServerURL: remote.URL, APIKey: "secret", Default: true,
			Projects: []string{"remote-project"},
		},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"content":  "decision: keep durable remote retries stable and observable",
		"category": "decision",
		"cwd":      repo,
	})
	output, err := srv.toolSave(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "(auto-sync queued)") {
		t.Fatalf("save output = %q, want durable remote queue confirmation", output)
	}
	waitForOutboxState(t, svc, "delivered", 4*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("idempotency keys = %q, want two identical non-empty keys", keys)
	}
	if len(payload) != 2 || payload[0] != payload[1] {
		t.Fatalf("payload snapshots changed across retry: %q", payload)
	}
	if strings.Contains(payload[0], repo) {
		t.Fatalf("remote payload was not redacted: %s", payload[0])
	}
	var attempts int
	if err := svc.StoreDB().QueryRow(`SELECT attempts FROM remote_outbox`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("outbox attempts = %d, want 2", attempts)
	}
}

func TestToolSaveAutoSyncCommitsBeforeRemoteRouting(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/auto-local-first.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	routingStarted := make(chan struct{})
	releaseRouting := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-routingStarted:
		default:
			close(routingStarted)
		}
		select {
		case <-releaseRouting:
			http.Error(w, "routing unavailable", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	defer remote.Close()
	defer close(releaseRouting)

	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "auto-local-first.db")
	cfg.Embedding.Provider = "none"
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {
			Name: "team", ServerURL: remote.URL, Default: true,
		},
	}
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"content":  "decision: commit locally before automatic routing",
		"category": "decision",
		"cwd":      repo,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	output, err := srv.toolSave(ctx, args)
	if err != nil {
		t.Fatalf("auto-sync save failed before local commit: %v", err)
	}
	if !strings.Contains(output, "(auto-sync queued)") {
		t.Fatalf("save output = %q, want queued routing", output)
	}

	var memories, queued int
	if err := svc.StoreDB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if err := svc.StoreDB().QueryRow(`SELECT COUNT(*) FROM remote_outbox`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if memories != 1 || queued != 1 {
		t.Fatalf("local commit = memories:%d outbox:%d, want 1/1", memories, queued)
	}

	select {
	case <-routingStarted:
	case <-time.After(time.Second):
		t.Fatal("durable worker did not attempt post-commit routing")
	}
}

func TestToolSaveExplicitRemoteRemainsSynchronous(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/explicit-fixture.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "explicit.db")
	cfg.Embedding.Provider = "none"
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil {
		t.Fatalf("resolve project = (%#v, %v)", proj, err)
	}

	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/projects":
			fmt.Fprintf(w, `[{"id":"remote-project","name":"explicit","slug":"explicit","remote_key":%q}]`, proj.RemoteKey)
		case "/v1/memories":
			calls.Add(1)
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("explicit remote save omitted idempotency key")
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"remote-memory","category":"decision","project_id":"remote-project","created":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {
			Name: "team", ServerURL: remote.URL, APIKey: "secret", Default: true,
			Projects: []string{"remote-project"},
		},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"content":  "decision: explicit remote saves are synchronous",
		"category": "decision",
		"cwd":      repo,
		"remote":   "team",
	})
	output, err := srv.toolSave(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "(remote: saved to team)") {
		t.Fatalf("save output = %q, want synchronous confirmation", output)
	}
	if calls.Load() != 1 {
		t.Fatalf("remote calls = %d, want 1 before toolSave returns", calls.Load())
	}
	var outboxRows int
	if err := svc.StoreDB().QueryRow(`SELECT COUNT(*) FROM remote_outbox`).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if outboxRows != 0 {
		t.Fatalf("explicit remote save queued %d outbox rows, want 0", outboxRows)
	}
}

func TestToolSaveExplicitRemoteCommitsLocalBeforeRemoteLookup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", "https://github.com/test/local-first-fixture.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "local-first.db")
	cfg.Embedding.Provider = "none"
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	var lookupSawLocal atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects" {
			http.NotFound(w, r)
			return
		}
		var count int
		if err := svc.StoreDB().QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
			t.Error(err)
		}
		lookupSawLocal.Store(count == 1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer remote.Close()
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, _ := json.Marshal(map[string]any{
		"content":  "decision: local commit precedes explicit remote lookup",
		"category": "decision",
		"cwd":      repo,
		"remote":   "team",
	})
	output, err := srv.toolSave(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !lookupSawLocal.Load() {
		t.Fatalf("remote lookup ran before local commit: %q", output)
	}
	if !strings.Contains(output, "no remote project resolved") {
		t.Fatalf("save output = %q", output)
	}
}

func TestRemoteOutboxDestinationSurvivesConfigRename(t *testing.T) {
	const endpoint = "https://memory.example.test"
	if got := remoteOutboxName(config.RemoteEntry{
		Name: "old-name", ServerURL: endpoint,
	}); got != endpoint {
		t.Fatalf("persisted destination = %q, want stable endpoint", got)
	}
	srv := &Server{cfg: &config.Config{Remotes: map[string]config.RemoteEntry{
		"new-name": {Name: "new-name", ServerURL: endpoint},
	}}}
	if entry := srv.remoteEntryForOutbox(endpoint); entry == nil || entry.Name != "new-name" {
		t.Fatalf("renamed remote did not resolve stored endpoint: %#v", entry)
	}
}

func TestRemoteSaveContentRedactsProjectRootBeforeSnapshot(t *testing.T) {
	content, ok := remoteSaveContent(memory.Memory{
		Category: "decision",
		Content:  "use /home/test/work/repo/config.yaml for the local fixture",
	}, "/home/test/work/repo", false)
	if !ok {
		t.Fatal("project-local path was unexpectedly blocked")
	}
	if strings.Contains(content, "/home/test/work/repo") || !strings.Contains(content, "./config.yaml") {
		t.Fatalf("redacted content = %q", content)
	}
}

func TestRemoteOutboxReconcilesAfterServiceRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "restart-outbox.db")
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = dbPath
	cfg.Embedding.Provider = "none"

	first, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"id":"restart-memory","project_id":"remote-project","category":"fact","content":"restart delivery"}`)
	if _, err := first.SaveDurable(context.Background(), memory.DurableSaveOptions{
		SaveOptions: memory.SaveOptions{
			Content:  "restart delivery",
			Category: "fact",
			Source:   "test",
		},
		RemoteOutbox: []memory.RemoteOutboxSpec{{
			Remote: "team", Project: "remote-project", Payload: payload,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	first.Close()

	var delivered atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"restart-memory","created":true}`))
	}))
	defer remote.Close()
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL},
	}
	reopened, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_ = NewServer(reopened, nil, nil, nil, cfg, "test", slog.Default())
	waitForOutboxState(t, reopened, "delivered", 3*time.Second)
	if delivered.Load() != 1 {
		t.Fatalf("restart deliveries = %d, want 1", delivered.Load())
	}
}

func waitForOutboxState(
	t *testing.T,
	svc *memory.Service,
	want string,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var state string
		err := svc.StoreDB().QueryRow(
			`SELECT state FROM remote_outbox ORDER BY created_at DESC LIMIT 1`,
		).Scan(&state)
		if err == nil && state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox state = %q, want %q (last error %v)", state, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
