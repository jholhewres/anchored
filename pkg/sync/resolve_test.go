package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// fakeProjectsServer answers GET /v1/projects with a single project carrying
// the given remote_key ("" → empty list).
func fakeProjectsServer(t *testing.T, remoteKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		if remoteKey == "" {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprintf(w, `[{"id":"proj-on-this-server","name":"x","slug":"x","remote_key":%q}]`, remoteKey)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestResolveProjectAcrossRemotes locks the "works right after configure"
// contract: a second server added with no routing paths must still be found
// when it is the one that knows the repository's git-origin key.
func TestResolveProjectAcrossRemotes(t *testing.T) {
	const key = "abc123key"
	personal := fakeProjectsServer(t, "") // default remote: does NOT know the repo
	company := fakeProjectsServer(t, key) // freshly-added remote: knows it

	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"default": {Name: "default", ServerURL: personal.URL, APIKey: "k1", Default: true},
		"company": {Name: "company", ServerURL: company.URL, APIKey: "k2"}, // no paths, no default
	}

	entry, pid, matched := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key)
	if entry == nil || entry.Name != "company" || pid != "proj-on-this-server" || matched != key {
		t.Fatalf("probe should find the company remote, got entry=%+v pid=%q matched=%q", entry, pid, matched)
	}

	// When the resolved (default) remote knows the key, it wins — the probe
	// must not jump to another server.
	both := fakeProjectsServer(t, key)
	cfg.Remotes["default"] = config.RemoteEntry{Name: "default", ServerURL: both.URL, APIKey: "k1", Default: true}
	entry, _, _ = ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key)
	if entry == nil || entry.Name != "default" {
		t.Fatalf("resolved remote should win when it knows the key, got %+v", entry)
	}

	// Unknown key everywhere → nothing.
	if e, p, m := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", "nope"); e != nil || p != "" || m != "" {
		t.Fatalf("unknown key should resolve to nothing, got %+v %q %q", e, p, m)
	}
}

// TestResolveProjectAcrossRemotes_ProbesConcurrently locks the latency
// contract: once the routed remote is late, the others are asked alongside
// it, yet the routed remote still wins over a faster one that knows the same
// key.
func TestResolveProjectAcrossRemotes_ProbesConcurrently(t *testing.T) {
	const key = "abc123key"
	var arrived sync.WaitGroup
	arrived.Add(2)
	bothArrived := make(chan struct{})
	go func() {
		arrived.Wait()
		close(bothArrived)
	}()

	var concurrent atomic.Bool
	server := func(projectID string, wait bool) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
			arrived.Done()
			if wait {
				// Answer only once the other probe is in flight too: a
				// sequential probe would never get here before the timeout.
				select {
				case <-bothArrived:
					concurrent.Store(true)
				case <-time.After(2 * time.Second):
				}
			}
			if _, err := fmt.Fprintf(w, `[{"id":%q,"name":"x","slug":"x","remote_key":%q}]`, projectID, key); err != nil {
				t.Errorf("write projects: %v", err)
			}
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		return ts
	}
	routed := server("routed-project", true)
	other := server("other-project", false)

	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"routed": {Name: "routed", ServerURL: routed.URL, APIKey: "k1", Default: true},
		"other":  {Name: "other", ServerURL: other.URL, APIKey: "k2"},
	}

	entry, pid, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key)
	if entry == nil || entry.Name != "routed" || pid != "routed-project" {
		t.Fatalf("routed remote should win even when it answers last, got entry=%+v pid=%q", entry, pid)
	}
	if !concurrent.Load() {
		t.Fatal("remotes were probed one after another")
	}
}

// TestResolveProjectAcrossRemotes_DeadRoutedRemoteDoesNotStall locks the
// per-probe deadline: a routed remote that never answers is given up on
// after remoteProbeTimeout, so a later remote that knows the key still wins
// quickly instead of after the HTTP client's 30 s timeout.
func TestResolveProjectAcrossRemotes_DeadRoutedRemoteDoesNotStall(t *testing.T) {
	// A dead routed remote is given up on after the probe deadline; the next
	// remote is asked once the hedge opens, before that deadline, so its
	// answer is ready when the routed probe ends.
	if remoteProbeTimeout > 3*time.Second || remoteHedgeDelay >= remoteProbeTimeout {
		t.Fatalf("probe deadline %s, hedge %s: want deadline <= 3s and hedge before it", remoteProbeTimeout, remoteHedgeDelay)
	}
	// Scaled down, the next remote answers after the hedge and before the
	// probe deadline: asked alongside the routed one, it is ready when that
	// deadline ends; asked only after it, the answer comes too late.
	prevProbe, prevHedge := remoteProbeTimeout, remoteHedgeDelay
	remoteProbeTimeout, remoteHedgeDelay = 600*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { remoteProbeTimeout, remoteHedgeDelay = prevProbe, prevHedge })

	const key = "abc123key"
	hung := http.NewServeMux()
	hung.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	dead := httptest.NewServer(hung)
	t.Cleanup(dead.Close)
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		if _, err := fmt.Fprintf(w, `[{"id":"proj-on-this-server","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
			t.Errorf("write projects: %v", err)
		}
	}))
	t.Cleanup(alive.Close)

	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"routed": {Name: "routed", ServerURL: dead.URL, APIKey: "k1", Default: true},
		"other":  {Name: "other", ServerURL: alive.URL, APIKey: "k2"},
	}

	start := time.Now()
	entry, pid, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key)
	elapsed := time.Since(start)
	if entry == nil || entry.Name != "other" || pid != "proj-on-this-server" {
		t.Fatalf("the live remote should answer, got entry=%+v pid=%q", entry, pid)
	}
	if elapsed > remoteProbeTimeout+180*time.Millisecond {
		t.Fatalf("a dead routed remote stalled the probe for %s", elapsed)
	}
}

// TestResolveProjectAcrossRemotesDetailed_Degraded locks what counts as a
// degraded answer: a higher-precedence remote that failed, never one that
// answered it does not know the repository.
func TestResolveProjectAcrossRemotesDetailed_Degraded(t *testing.T) {
	const key = "abc123key"
	failing := http.NewServeMux()
	failing.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	down := httptest.NewServer(failing)
	t.Cleanup(down.Close)
	unaware := fakeProjectsServer(t, "")
	aware := fakeProjectsServer(t, key)

	for _, tc := range []struct {
		name     string
		routed   string
		degraded bool
	}{
		{"routed remote does not know the repo", unaware.URL, false},
		{"routed remote failed to answer", down.URL, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Remotes = map[string]config.RemoteEntry{
				"routed": {Name: "routed", ServerURL: tc.routed, APIKey: "k1", Default: true},
				"other":  {Name: "other", ServerURL: aware.URL, APIKey: "k2"},
			}
			r := ResolveProjectAcrossRemotesDetailed(context.Background(), cfg, "/some/repo", "test", remoteProbeTimeout, key)
			if r.Entry == nil || r.Entry.Name != "other" {
				t.Fatalf("entry = %+v, want other", r.Entry)
			}
			if r.Degraded != tc.degraded {
				t.Fatalf("Degraded = %v, want %v", r.Degraded, tc.degraded)
			}
		})
	}
}

// TestResolveProjectAcrossRemotes_CancelsLosingProbes locks the cleanup after
// an answer: the probes still in flight are cancelled instead of holding a
// request open until their own deadline.
func TestResolveProjectAcrossRemotes_CancelsLosingProbes(t *testing.T) {
	prev := remoteHedgeDelay
	remoteHedgeDelay = 20 * time.Millisecond
	t.Cleanup(func() { remoteHedgeDelay = prev })

	const key = "abc123key"
	arrived := make(chan struct{})
	cancelled := make(chan struct{})
	slow := http.NewServeMux()
	slow.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-r.Context().Done()
		close(cancelled)
	})
	hanging := httptest.NewServer(slow)
	t.Cleanup(hanging.Close)
	// The routed remote answers only once the other probe is in flight.
	late := http.NewServeMux()
	late.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-arrived:
		case <-time.After(time.Second):
		}
		if _, err := fmt.Fprintf(w, `[{"id":"proj-on-this-server","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
			t.Errorf("write projects: %v", err)
		}
	})
	routed := httptest.NewServer(late)
	t.Cleanup(routed.Close)

	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"routed": {Name: "routed", ServerURL: routed.URL, APIKey: "k1", Default: true},
		"other":  {Name: "other", ServerURL: hanging.URL, APIKey: "k2"},
	}
	if entry, _, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key); entry == nil || entry.Name != "routed" {
		t.Fatalf("entry = %+v, want routed", entry)
	}
	<-arrived
	select {
	case <-cancelled:
	case <-time.After(remoteProbeTimeout / 2):
		t.Fatal("the losing probe was still running after the answer")
	}
}

// TestResolveProjectAcrossRemotes_RoutedAnswerSparesOtherRemotes locks the
// hedge: when the routed remote knows the repository and answers quickly, no
// other server is asked, so it never learns when this client is at work.
func TestResolveProjectAcrossRemotes_RoutedAnswerSparesOtherRemotes(t *testing.T) {
	const key = "abc123key"
	routed := fakeProjectsServer(t, key)
	var asked atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write projects: %v", err)
		}
	})
	other := httptest.NewServer(mux)
	t.Cleanup(other.Close)

	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"routed": {Name: "routed", ServerURL: routed.URL, APIKey: "k1", Default: true},
		"other":  {Name: "other", ServerURL: other.URL, APIKey: "k2"},
	}
	if entry, _, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key); entry == nil || entry.Name != "routed" {
		t.Fatalf("entry = %+v, want routed", entry)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the other remote was asked %d times, want 0", n)
	}
}

// TestResolveProjectAcrossRemotesForWrite_WaitsForSlowRemote locks the write
// rule: flows that send data run in the background and wait for a slow remote
// instead of treating it as failed after the interactive probe deadline.
func TestResolveProjectAcrossRemotesForWrite_WaitsForSlowRemote(t *testing.T) {
	prev := remoteProbeTimeout
	remoteProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { remoteProbeTimeout = prev })

	const key = "abc123key"
	slow := http.NewServeMux()
	slow.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * remoteProbeTimeout)
		if _, err := fmt.Fprintf(w, `[{"id":"proj-on-this-server","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
			t.Errorf("write projects: %v", err)
		}
	})
	ts := httptest.NewServer(slow)
	t.Cleanup(ts.Close)
	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"only": {Name: "only", ServerURL: ts.URL, APIKey: "k1", Default: true},
	}

	if entry, _, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/some/repo", "test", key); entry != nil {
		t.Fatalf("interactive probe = %+v, want nothing past its deadline", entry)
	}
	if r := ResolveProjectAcrossRemotesForWrite(context.Background(), cfg, "/some/repo", "test", key); r.Entry == nil || r.Entry.Name != "only" || r.ProjectID != "proj-on-this-server" {
		t.Fatalf("write probe = %+v %q, want the slow remote", r.Entry, r.ProjectID)
	}
}

// TestResolveProjectAcrossRemotesForWrite_StrictPrecedence locks the write
// rule: a write never passes over a higher-precedence remote that did not
// answer, since that remote may own the repository. It is held instead, and
// the remotes after it are not asked. A remote that answers it does not know
// the repository is passed over as usual.
func TestResolveProjectAcrossRemotesForWrite_StrictPrecedence(t *testing.T) {
	const key = "abc123key"
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	unaware := fakeProjectsServer(t, "")
	var asked atomic.Int32
	aware := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if _, err := fmt.Fprintf(w, `[{"id":"proj-on-this-server","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
			t.Errorf("write projects: %v", err)
		}
	}))
	t.Cleanup(aware.Close)

	resolve := func(routed string) ProjectResolution {
		cfg := config.Defaults()
		cfg.Remotes = map[string]config.RemoteEntry{
			"routed": {Name: "routed", ServerURL: routed, APIKey: "k1", Default: true},
			"other":  {Name: "other", ServerURL: aware.URL, APIKey: "k2"},
		}
		return ResolveProjectAcrossRemotesForWrite(context.Background(), cfg, "/some/repo", "test", key)
	}

	if r := resolve(down.URL); r.Entry != nil || !r.Held() {
		t.Fatalf("behind a failed remote: entry=%+v held=%v, want held", r.Entry, r.Held())
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the remote after the failed one was asked %d times, want 0", n)
	}
	if r := resolve(unaware.URL); r.Entry == nil || r.Entry.Name != "other" || r.Held() {
		t.Fatalf("behind a remote that does not know the repo: entry=%+v held=%v, want other", r.Entry, r.Held())
	}
}

// TestResolveProjectAcrossRemotesForWrite_SaysWhoHeldIt locks what a held
// write reports: the remote that held it and how it answered, so an auth
// failure reads as "log in again" instead of an outage, and a remote that
// hangs holds the write within the write deadline instead of the HTTP
// client's.
func TestResolveProjectAcrossRemotesForWrite_SaysWhoHeldIt(t *testing.T) {
	prev := remoteWriteProbeTimeout
	remoteWriteProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { remoteWriteProbeTimeout = prev })

	const key = "abc123key"
	status := func(code int) *httptest.Server {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(code), code)
		}))
		t.Cleanup(ts.Close)
		return ts
	}
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hung.Close)
	aware := fakeProjectsServer(t, key)

	for _, tc := range []struct {
		name   string
		routed string
		status int
		reason string
	}{
		{"server error", status(http.StatusServiceUnavailable).URL, http.StatusServiceUnavailable, "did not answer"},
		{"auth refused", status(http.StatusUnauthorized).URL, http.StatusUnauthorized, "log in"},
		{"hanging", hung.URL, 0, "did not answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Remotes = map[string]config.RemoteEntry{
				"routed": {Name: "routed", ServerURL: tc.routed, APIKey: "k1", Default: true},
				"other":  {Name: "other", ServerURL: aware.URL, APIKey: "k2"},
			}
			start := time.Now()
			r := ResolveProjectAcrossRemotesForWrite(context.Background(), cfg, "/some/repo", "test", key)
			if !r.Held() || r.HeldBy != "routed" || r.HeldStatus != tc.status {
				t.Fatalf("held=%v by=%q status=%d, want held by routed with %d", r.Held(), r.HeldBy, r.HeldStatus, tc.status)
			}
			if reason := r.HeldReason(); !strings.Contains(reason, "routed") || !strings.Contains(reason, tc.reason) {
				t.Fatalf("HeldReason() = %q, want it to name routed and say %q", reason, tc.reason)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("held after %s, want within the write deadline", elapsed)
			}
		})
	}
}

// An explicit path glob names the remote a repository writes to. Another
// remote that lists a project under the repository's key (keys come from the
// public origin URL, so anyone on a shared server can register one) does not
// take its writes; it is not even asked. Without a glob the probe still finds
// the remote that knows the repository.
func TestResolveProjectAcrossRemotesForWrite_ExplicitGlobWins(t *testing.T) {
	const key = "abc123key"
	unaware := fakeProjectsServer(t, "")
	var asked atomic.Int32
	claimant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if _, err := fmt.Fprintf(w, `[{"id":"claimed","name":"x","slug":"x","remote_key":%q}]`, key); err != nil {
			t.Errorf("write projects: %v", err)
		}
	}))
	t.Cleanup(claimant.Close)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)

	resolve := func(work string, paths ...string) ProjectResolution {
		cfg := config.Defaults()
		cfg.Remotes = map[string]config.RemoteEntry{
			"work":   {Name: "work", ServerURL: work, APIKey: "k1", Paths: paths},
			"shared": {Name: "shared", ServerURL: claimant.URL, APIKey: "k2", Default: true},
		}
		return ResolveProjectAcrossRemotesForWrite(context.Background(), cfg, "/home/me/work/api", "test", key)
	}

	if r := resolve(unaware.URL, "/home/me/work/*"); r.Entry != nil || r.Held() {
		t.Fatalf("glob-routed write went to %+v (held=%v), want nothing: the glob's remote does not know the repo yet", r.Entry, r.Held())
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("a remote the glob does not name was asked %d times", n)
	}
	if r := resolve(down.URL, "/home/me/work/*"); !r.Held() || r.HeldBy != "work" {
		t.Fatalf("behind a silent glob remote: %+v, want held by work", r)
	}
	if r := resolve(unaware.URL); r.Entry == nil || r.Entry.Name != "shared" || r.ProjectID != "claimed" {
		t.Fatalf("without a glob: %+v, want the probe to find shared", r)
	}

	// Reads follow the glob as well: the claimant's hits and skills never
	// reach a repository the glob routes elsewhere.
	asked.Store(0)
	cfg := config.Defaults()
	cfg.Remotes = map[string]config.RemoteEntry{
		"work":   {Name: "work", ServerURL: unaware.URL, APIKey: "k1", Paths: []string{"/home/me/work/*"}},
		"shared": {Name: "shared", ServerURL: claimant.URL, APIKey: "k2", Default: true},
	}
	if entry, pid, _ := ResolveProjectAcrossRemotes(context.Background(), cfg, "/home/me/work/api", "test", key); entry != nil || pid != "" || asked.Load() != 0 {
		t.Fatalf("glob-routed read found %+v %q and asked the claimant %d times, want nothing and 0", entry, pid, asked.Load())
	}
}
