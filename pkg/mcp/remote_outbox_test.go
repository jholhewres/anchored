package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
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

// TestRemoteOutboxHoldsAutoRouteBehindDownRemote locks the outage path end to
// end: an automatic save whose routed remote is down stays in the outbox as
// held, with no attempt spent, instead of failing toward the dead letter.
func TestRemoteOutboxHoldsAutoRouteBehindDownRemote(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: down.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{
		"content":  "decision: hold writes while the routed remote is down",
		"category": "decision",
		"cwd":      repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSave(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	state, attempts, errorClass := waitForNewestOutboxRow(t, svc, 5*time.Second, func(_ string, _ int, errorClass string) bool {
		return errorClass != ""
	})
	if state != "pending" || attempts != 0 || errorClass != "held" {
		t.Fatalf("outbox item: state=%s attempts=%d error_class=%s, want pending, 0, held", state, attempts, errorClass)
	}
}

// TestRemoteOutboxRechecksRoutingAfterDeliveryFailure locks recovery from a
// stale routing answer: when an automatic delivery fails because the routed
// remote went down after it was resolved, the next attempt resolves again and
// holds the item, instead of spending its attempts against the cached answer.
func TestRemoteOutboxRechecksRoutingAfterDeliveryFailure(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	var down atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/projects" {
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		// The remote goes down while the first delivery is on its way.
		down.Store(true)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{
		"content":  "decision: recheck routing after a failed delivery",
		"category": "decision",
		"cwd":      repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSave(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	// A claim counts the attempt before delivery, so a row in processing is
	// mid-attempt; judge it once the attempt has settled.
	state, attempts, errorClass := waitForNewestOutboxRow(t, svc, 8*time.Second, func(state string, attempts int, errorClass string) bool {
		return state != "processing" && (errorClass == "held" || state == "dead_letter" || attempts > 1)
	})
	if errorClass != "held" || attempts > 1 {
		t.Fatalf("after the remote went down: state=%s attempts=%d error_class=%s, want held with at most the first attempt spent", state, attempts, errorClass)
	}
}

// waitForNewestOutboxRow polls the newest outbox row until done accepts it,
// and returns that row.
func waitForNewestOutboxRow(t *testing.T, svc *memory.Service, timeout time.Duration, done func(state string, attempts int, errorClass string) bool) (string, int, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var state, errorClass string
		var attempts int
		err := svc.StoreDB().QueryRow(`SELECT state, attempts, COALESCE(error_class, '') FROM remote_outbox ORDER BY created_at DESC LIMIT 1`).Scan(&state, &attempts, &errorClass)
		if err == nil && done(state, attempts, errorClass) {
			return state, attempts, errorClass
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox row never settled: state=%s attempts=%d error_class=%s err=%v", state, attempts, errorClass, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRemoteOutboxHoldsWhenTheRoutedRemoteRefusesCredentials locks the hold
// for a refused credential at delivery time: with the routing already cached,
// a 401 holds the item with the "log in again" reason and drops the cached
// routing, instead of dead-lettering it.
func TestRemoteOutboxHoldsWhenTheRoutedRemoteRefusesCredentials(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects" {
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{
		"content":  "decision: hold writes when the remote refuses the credentials",
		"category": "decision",
		"cwd":      repo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSave(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	state, _, errorClass := waitForNewestOutboxRow(t, svc, 5*time.Second, func(state string, _ int, errorClass string) bool {
		return state != "processing" && errorClass != ""
	})
	var lastError string
	if err := svc.StoreDB().QueryRow(`SELECT COALESCE(last_error, '') FROM remote_outbox ORDER BY created_at DESC LIMIT 1`).Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || errorClass != "held" || !strings.Contains(lastError, "log in") {
		t.Fatalf("after a 401 on delivery: state=%s error_class=%s last_error=%q, want pending, held, log in again", state, errorClass, lastError)
	}
}

// TestRemoteOutboxDeliveryRefusalHoldsTheRepository locks the cost of a
// refusal at delivery: once the routed remote refuses a save, the items
// queued after it for the same repository are held without another listing
// or send until the hold expires.
func TestRemoteOutboxDeliveryRefusalHoldsTheRepository(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	var listings, saves atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects" {
			listings.Add(1)
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		saves.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	save := func(content string) {
		t.Helper()
		args, err := json.Marshal(map[string]any{"content": content, "category": "decision", "cwd": repo})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.toolSave(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	save("decision: hold the first write when the routed remote refuses the save credentials")
	waitForNewestOutboxRow(t, svc, 5*time.Second, func(state string, _ int, errorClass string) bool {
		return state != "processing" && errorClass == "held"
	})
	// A search in between lists the projects again; the hold still stands.
	srv.resolveAutoRemoteTarget(context.Background(), repo)
	save("decision: hold the second write for the same repository without asking the remote again")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var held int
		if err := svc.StoreDB().QueryRow(`SELECT COUNT(*) FROM remote_outbox WHERE error_class = 'held' AND state = 'pending'`).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 2 items held", held)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sv := saves.Load(); sv != 1 {
		t.Fatalf("two queued writes cost %d sends (and %d listings), want 1 send", sv, listings.Load())
	}
}

// TestRemoteOutboxRefusalOutsideARepositoryIsNotHeld locks the bound on the
// hold: outside a git repository there is no repository to hold, so a save
// refused with 401/403 fails as before, toward the dead letter, instead of
// being resent every minute forever.
func TestRemoteOutboxRefusalOutsideARepositoryIsNotHeld(t *testing.T) {
	dir := t.TempDir()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(remote.Close)
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "refusal.db")
	cfg.Embedding.Provider = "none"
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true, Projects: []string{"rp-team"}},
	}
	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	payload, err := json.Marshal(remoteOutboxEnvelope{
		Kind: autoRouteOutboxKind,
		CWD:  dir,
		Memory: remotesync.RemoteMemory{
			ID: "m-outside", Category: "decision", Content: "decision: outside any repository", Source: "mcp",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := srv.saveRemoteIdempotent(context.Background(), memory.RemoteOutboxItem{
		OperationID: "op-outside", Remote: autoRouteOutboxKind, Payload: payload,
	})
	if result.Held || result.StatusCode != http.StatusForbidden {
		t.Fatalf("refusal outside a repository: held=%v status=%d, want a 403 failure", result.Held, result.StatusCode)
	}
}

// TestRemoteOutboxFollowsAnExplicitGlob locks the write rule for automatic
// saves: a path glob names the repository's remote, so a remote that only
// lists a project under the repository's key receives nothing.
func TestRemoteOutboxFollowsAnExplicitGlob(t *testing.T) {
	t.Run("from the repository root", func(t *testing.T) { testRemoteOutboxFollowsAnExplicitGlob(t, "") })
	// The model may pass any directory inside the repository.
	t.Run("from a subdirectory", func(t *testing.T) { testRemoteOutboxFollowsAnExplicitGlob(t, "internal/billing") })
}

func testRemoteOutboxFollowsAnExplicitGlob(t *testing.T, sub string) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	cwd := filepath.Join(repo, sub)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	work := fakeProjectsListing(t, "[]", nil)
	var delivered atomic.Int32
	shared := fakeProjectsListing(t, fmt.Sprintf(`[{"id":"claimed","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey), &delivered)
	cfg.Remotes = map[string]config.RemoteEntry{
		"work":   {Name: "work", ServerURL: work.URL, APIKey: "k1", Paths: []string{filepath.Dir(repo) + "/*"}},
		"shared": {Name: "shared", ServerURL: shared.URL, APIKey: "k2", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	// A read first, as a search would: its cached answer must not route the
	// write past the glob.
	if target, _ := srv.resolveAutoRemoteTarget(context.Background(), cwd); target != nil && target.Name != "work" {
		t.Fatalf("glob-routed read resolved to %q", target.Name)
	}

	args, err := json.Marshal(map[string]any{
		"content":  "decision: an explicit glob names the remote for automatic saves",
		"category": "decision",
		"cwd":      cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSave(context.Background(), args); err != nil {
		t.Fatal(err)
	}

	waitForNewestOutboxRow(t, svc, 5*time.Second, func(state string, attempts int, _ string) bool {
		return state != "processing" && attempts >= 1
	})
	if n := delivered.Load(); n != 0 {
		t.Fatalf("shared, which only lists the repository's key, received %d deliveries", n)
	}
}

// fakeProjectsListing answers GET /v1/projects with listing and counts every
// other request in delivered (when not nil), answering it with an empty
// success.
func fakeProjectsListing(t *testing.T, listing string, delivered *atomic.Int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/projects" {
			if _, err := fmt.Fprint(w, listing); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		if delivered != nil {
			delivered.Add(1)
		}
		if _, err := fmt.Fprint(w, `{}`); err != nil {
			t.Errorf("write reply: %v", err)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRemoteOutboxStoresAnAbsoluteCWD locks where an automatic save is routed
// from: a save with cwd omitted records the absolute directory, since the
// process that delivers the row may run somewhere else.
func TestRemoteOutboxStoresAnAbsoluteCWD(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	remote := fakeProjectsListing(t, "[]", nil)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	t.Chdir(repo)

	args, err := json.Marshal(map[string]any{
		"content":  "decision: route an automatic save from where it was made",
		"category": "decision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSave(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := svc.StoreDB().QueryRow(`SELECT payload_snapshot FROM remote_outbox ORDER BY created_at DESC LIMIT 1`).Scan(&payload); err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	var envelope remoteOutboxEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.CWD != repo {
		t.Fatalf("outbox envelope cwd = %q, want %q", envelope.CWD, repo)
	}
}
