package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// TestResolveAutoRemoteTarget_CachesResolution locks the per-call cost: the
// automatic flows resolve the repository's remote once, not on every call,
// and a configuration change resolves again instead of reusing the answer.
func TestResolveAutoRemoteTarget_CachesResolution(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}

	remote := func(projectID string, listings *atomic.Int32) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
			listings.Add(1)
			if _, err := fmt.Fprintf(w, `[{"id":%q,"name":"x","slug":"x","remote_key":%q}]`, projectID, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return ts
	}
	var firstListings, secondListings atomic.Int32
	first := remote("rp-first", &firstListings)
	second := remote("rp-second", &secondListings)

	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: first.URL, APIKey: "k", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		target, pid := srv.resolveAutoRemoteTarget(ctx, repo)
		if target == nil || target.Name != "team" || pid != "rp-first" {
			t.Fatalf("call %d: got target=%+v pid=%q", i, target, pid)
		}
	}
	if got := firstListings.Load(); got != 1 {
		t.Errorf("projects listed %d times for three calls, want 1", got)
	}

	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: second.URL, APIKey: "k", Default: true},
	}
	if _, pid := srv.resolveAutoRemoteTarget(ctx, repo); pid != "rp-second" {
		t.Errorf("after the remote changed, pid = %q, want rp-second", pid)
	}
	if got := secondListings.Load(); got != 1 {
		t.Errorf("new remote listed %d times, want 1", got)
	}
}

// TestResolveAutoRemoteTarget_DegradedAnswerExpiresSoon locks precedence after
// an outage: while the routed remote fails to answer, another remote that
// knows the repository takes the traffic, but only until the short miss TTL
// runs out, so the routed remote wins again once it is back.
func TestResolveAutoRemoteTarget_DegradedAnswerExpiresSoon(t *testing.T) {
	if remoteReadProbeTimeout > 3*time.Second {
		t.Fatalf("remoteReadProbeTimeout = %s, want at most 3s for interactive reads", remoteReadProbeTimeout)
	}
	prev := remoteMissTTL
	remoteMissTTL = 50 * time.Millisecond
	t.Cleanup(func() { remoteMissTTL = prev })

	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	remote := func(projectID string, failFirst bool) *httptest.Server {
		var calls atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 && failFirst {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if _, err := fmt.Fprintf(w, `[{"id":%q,"name":"x","slug":"x","remote_key":%q}]`, projectID, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return ts
	}
	routed := remote("rp-routed", true)
	other := remote("rp-other", false)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team":  {Name: "team", ServerURL: routed.URL, APIKey: "k1", Default: true},
		"other": {Name: "other", ServerURL: other.URL, APIKey: "k2"},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	ctx := context.Background()

	if target, pid := srv.resolveAutoRemoteTarget(ctx, repo); target == nil || target.Name != "other" || pid != "rp-other" {
		t.Fatalf("during the outage: got target=%+v pid=%q, want other", target, pid)
	}
	time.Sleep(2 * remoteMissTTL)
	if target, pid := srv.resolveAutoRemoteTarget(ctx, repo); target == nil || target.Name != "team" || pid != "rp-routed" {
		t.Fatalf("after the routed remote recovered: got target=%+v pid=%q, want team", target, pid)
	}
}

// TestResolveAutoRemoteWriteTarget_IgnoresDegradedReadAnswer locks the write
// rule. A read answer given while the routed remote was slow is not reused for
// writes, which wait for the routed remote. When the routed remote is down,
// reads may use another remote that knows the repository, but writes are held
// until it answers.
func TestResolveAutoRemoteWriteTarget_IgnoresDegradedReadAnswer(t *testing.T) {
	prev := remoteReadProbeTimeout
	remoteReadProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { remoteReadProbeTimeout = prev })

	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	knows := func(projectID string, delay time.Duration) *httptest.Server {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			if _, err := fmt.Fprintf(w, `[{"id":%q,"name":"x","slug":"x","remote_key":%q}]`, projectID, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
		}))
		t.Cleanup(ts.Close)
		return ts
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	other := knows("rp-other", 0)

	t.Run("slow routed remote", func(t *testing.T) {
		cfg.Remotes = map[string]config.RemoteEntry{
			"team":  {Name: "team", ServerURL: knows("rp-team", 300*time.Millisecond).URL, APIKey: "k1", Default: true},
			"other": {Name: "other", ServerURL: other.URL, APIKey: "k2"},
		}
		srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
		if target, _ := srv.resolveAutoRemoteTarget(context.Background(), repo); target == nil || target.Name != "other" {
			t.Fatalf("read target = %+v, want other while team is slow", target)
		}
		if r := srv.resolveAutoRemoteWriteTarget(context.Background(), repo); r.Entry == nil || r.Entry.Name != "team" || r.ProjectID != "rp-team" || r.Held() {
			t.Fatalf("write target = %+v %q held=%v, want team", r.Entry, r.ProjectID, r.Held())
		}
	})
	t.Run("routed remote down", func(t *testing.T) {
		cfg.Remotes = map[string]config.RemoteEntry{
			"team":  {Name: "team", ServerURL: down.URL, APIKey: "k1", Default: true},
			"other": {Name: "other", ServerURL: other.URL, APIKey: "k2"},
		}
		srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
		if target, _ := srv.resolveAutoRemoteTarget(context.Background(), repo); target == nil || target.Name != "other" {
			t.Fatalf("read target = %+v, want other while team is down", target)
		}
		if r := srv.resolveAutoRemoteWriteTarget(context.Background(), repo); r.Entry != nil || r.ProjectID != "" || !r.Held() {
			t.Fatalf("write target = %+v %q held=%v, want held while team is down", r.Entry, r.ProjectID, r.Held())
		}
	})
}

// TestResolveAutoRemoteWriteTarget_CachesHeldAnswer locks the cost of an
// outage: once a write is held behind a remote that is not answering, the
// writes queued after it for the same repository are held without asking
// that remote again until the hold expires.
func TestResolveAutoRemoteWriteTarget_CachesHeldAnswer(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	var asked atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: down.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if r := srv.resolveAutoRemoteWriteTarget(ctx, repo); !r.Held() {
			t.Fatalf("write %d: not held while team is down", i)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("the down remote was asked %d times for three writes, want 1", n)
	}
}

// TestResolveAutoRemoteTarget_ReadMissIsNotAWriteHold locks the line between
// reads and writes when the only remote fails: a search's failed probe is
// cached for later searches, and a write still probes on its own, so a
// refused credential reads as "log in again" with the remote's name.
func TestResolveAutoRemoteTarget_ReadMissIsNotAWriteHold(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	var asked atomic.Int32
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(refused.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: refused.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if target, _ := srv.resolveAutoRemoteTarget(ctx, repo); target != nil {
			t.Fatalf("search %d resolved %+v, want nothing", i, target)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("three searches asked the failing remote %d times, want 1", n)
	}
	r := srv.resolveAutoRemoteWriteTarget(ctx, repo)
	if !r.Held() || r.HeldBy != "team" || r.HeldStatus != http.StatusUnauthorized || !strings.Contains(r.HeldReason(), "log in") {
		t.Fatalf("write after the searches: held=%v by=%q status=%d reason=%q", r.Held(), r.HeldBy, r.HeldStatus, r.HeldReason())
	}
}

// TestToolSearch_FailedRemoteSearchDropsTheCachedRouting locks recovery on the
// read path: when the merge's search fails on the cached remote, the next
// search resolves again instead of trusting that answer for five minutes.
func TestToolSearch_FailedRemoteSearchDropsTheCachedRouting(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	var listings atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects" {
			listings.Add(1)
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())

	args, err := json.Marshal(map[string]any{"query": "fallback fixture", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.toolSearch(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	srv.resolveAutoRemoteTarget(context.Background(), repo)
	if n := listings.Load(); n != 2 {
		t.Fatalf("projects listed %d times, want 2: a failed remote search should drop the cached routing", n)
	}
}

// TestToolSearch_FailedSearchKeepsTheWriteHold locks the scope of that drop:
// a rate-limited search leaves the routing alone, and a failed search drops
// only the read answer, never a hold that writes are waiting behind.
func TestToolSearch_FailedSearchKeepsTheWriteHold(t *testing.T) {
	repo, svc, cfg := newRemoteSearchFixture(t)
	proj, err := svc.ResolveProjectInfo(repo)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		t.Fatalf("ResolveProjectInfo: proj=%v err=%v", proj, err)
	}
	var listings atomic.Int32
	var searchStatus atomic.Int32
	searchStatus.Store(http.StatusTooManyRequests)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/projects" {
			listings.Add(1)
			if _, err := fmt.Fprintf(w, `[{"id":"rp-team","name":"x","slug":"x","remote_key":%q}]`, proj.RemoteKey); err != nil {
				t.Errorf("write projects: %v", err)
			}
			return
		}
		http.Error(w, "busy", int(searchStatus.Load()))
	}))
	t.Cleanup(remote.Close)
	cfg.Remotes = map[string]config.RemoteEntry{
		"team": {Name: "team", ServerURL: remote.URL, APIKey: "k1", Default: true},
	}
	srv := NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
	ctx := context.Background()
	args, err := json.Marshal(map[string]any{"query": "fallback fixture", "cwd": repo})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srv.toolSearch(ctx, args); err != nil {
		t.Fatal(err)
	}
	srv.resolveAutoRemoteTarget(ctx, repo)
	if n := listings.Load(); n != 1 {
		t.Fatalf("a rate-limited search made the routing be listed %d times, want 1", n)
	}

	srv.holdAutoRemoteTarget(repo, remotesync.ProjectResolution{HeldBy: "team", HeldStatus: http.StatusForbidden})
	searchStatus.Store(http.StatusServiceUnavailable)
	if _, err := srv.toolSearch(ctx, args); err != nil {
		t.Fatal(err)
	}
	if r := srv.resolveAutoRemoteWriteTarget(ctx, repo); !r.Held() || r.HeldStatus != http.StatusForbidden {
		t.Fatalf("after a failed search: held=%v status=%d, want the 403 hold to stand", r.Held(), r.HeldStatus)
	}
}

// A path the glob covers and one only the default reaches can route to the
// same remote yet must not share a cached answer: the first asks only that
// remote, the second probes every remote, and its answer must not steer the
// first one's writes.
func TestRemoteResolutionKey_SeparatesGlobAndDefaultRoutes(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "work", "api")
	for _, dir := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "pkg"), filepath.Join(base, "elsewhere", "api")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"work":   {Name: "work", ServerURL: "https://w.example", APIKey: "k1", Default: true, Paths: []string{filepath.Join(base, "work") + "/*"}},
		"shared": {Name: "shared", ServerURL: "https://s.example", APIKey: "k2"},
	}
	byGlob := remoteResolutionKey(cfg, repo, "canon", "legacy")
	byDefault := remoteResolutionKey(cfg, filepath.Join(base, "elsewhere", "api"), "canon", "legacy")
	if byGlob == byDefault {
		t.Fatal("a glob-routed path and a default-routed path share a cache key")
	}
	if sub := remoteResolutionKey(cfg, filepath.Join(repo, "pkg"), "canon", "legacy"); sub != byGlob {
		t.Error("a subdirectory of a glob-routed repository got a different cache key than the repository")
	}
}
