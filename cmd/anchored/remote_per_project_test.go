package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	projectpkg "github.com/jholhewres/anchored/pkg/project"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// fakeRemote is an Anchored server that keeps its projects in memory: it lists
// and creates them (a taken slug fails, as on the real server) and records the
// memories each push sent, by project, and how many requests it served.
type fakeRemote struct {
	*httptest.Server
	mu       sync.Mutex
	projects []remotesync.RemoteProject
	creates  int
	lists    int
	requests int
	pushed   map[string][]string // project id -> memory contents
}

func newFakeRemote(t *testing.T, name string) *fakeRemote {
	t.Helper()
	f := &fakeRemote{pushed: map[string][]string{}}
	reply := func(w http.ResponseWriter, v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("fake remote %s: encode: %v", name, err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lists++
		reply(w, f.projects)
	})
	mux.HandleFunc("POST /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string `json:"name"`
			Slug      string `json:"slug"`
			RemoteKey string `json:"remote_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.projects {
			if p.Slug == in.Slug {
				http.Error(w, "duplicate slug", http.StatusInternalServerError)
				return
			}
		}
		f.creates++
		p := remotesync.RemoteProject{ID: fmt.Sprintf("%s-%d", name, f.creates), Name: in.Name, Slug: in.Slug, RemoteKey: in.RemoteKey}
		f.projects = append(f.projects, p)
		reply(w, p)
	})
	mux.HandleFunc("POST /api/v1/sync/push", func(w http.ResponseWriter, r *http.Request) {
		var req remotesync.SyncPushRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		known := false
		for _, p := range f.projects {
			// A push by project_claim resolves the project by its key, as
			// the server does.
			if req.ProjectID == "" && req.ProjectClaim != nil && p.RemoteKey == req.ProjectClaim.RemoteKey {
				req.ProjectID = p.ID
			}
			known = known || p.ID == req.ProjectID
		}
		if !known {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
		for _, m := range req.Memories {
			f.pushed[req.ProjectID] = append(f.pushed[req.ProjectID], m.Content)
		}
		reply(w, remotesync.SyncPushResponse{Accepted: len(req.Memories), ProjectID: req.ProjectID})
	})
	mux.HandleFunc("POST /v1/projects/{id}/triples", func(w http.ResponseWriter, r *http.Request) {
		reply(w, remotesync.SyncTripleResponse{})
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

type fakeRemoteState struct {
	projects []remotesync.RemoteProject
	creates  int
	lists    int
	requests int
	pushed   map[string][]string
}

func (f *fakeRemote) snapshot() fakeRemoteState {
	f.mu.Lock()
	defer f.mu.Unlock()
	pushed := map[string][]string{}
	for k, v := range f.pushed {
		pushed[k] = append([]string(nil), v...)
	}
	return fakeRemoteState{
		projects: append([]remotesync.RemoteProject(nil), f.projects...),
		creates:  f.creates,
		lists:    f.lists,
		requests: f.requests,
		pushed:   pushed,
	}
}

// gitRepo makes a git repository at dir with the given origin and returns its
// canonical git-origin key.
func gitRepo(t *testing.T, dir, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	key, _ := projectpkg.RemoteKeysFromDir(dir)
	if key == "" {
		t.Fatalf("%s has no git-origin key", dir)
	}
	return key
}

const (
	apiDecision   = "Decided that the api client retries failed uploads three times with exponential backoff before it gives up and reports"
	notesDecision = "Decided that the notes site is rendered by a static site generator at build time so the server only ever serves plain files"
)

// syncFixture is two repositories, work/api and home/notes, under base.
type syncFixture struct {
	base, apiDir, notesDir string
	apiKey, notesKey       string
}

func newSyncFixture(t *testing.T) *syncFixture {
	t.Helper()
	f := &syncFixture{base: t.TempDir()}
	f.apiDir = filepath.Join(f.base, "work", "api")
	f.notesDir = filepath.Join(f.base, "home", "notes")
	f.apiKey = gitRepo(t, f.apiDir, "https://github.com/acme/api.git")
	f.notesKey = gitRepo(t, f.notesDir, "https://github.com/me/notes.git")
	return f
}

// saveDecisions saves one decision per repository directory in the store the
// config points at.
func saveDecisions(t *testing.T, cfgPath string, byDir map[string]string) {
	t.Helper()
	_, _, svc, err := initService(cfgPath)
	if err != nil {
		t.Fatalf("initService: %v", err)
	}
	defer svc.Close()
	for dir, content := range byDir {
		if _, err := svc.SaveWithOptions(context.Background(), memory.SaveOptions{Content: content, Category: "decision", Source: "test", CWD: dir, SkipEmbed: true}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
}

// wantOnlyProject checks the remote holds exactly one project, keyed key,
// that received content and nothing else.
func wantOnlyProject(t *testing.T, run string, remote *fakeRemote, key, content, out string) {
	t.Helper()
	s := remote.snapshot()
	if s.creates != 1 || len(s.projects) != 1 || s.projects[0].RemoteKey != key {
		t.Fatalf("%s: want one project keyed %q, got %d creates, projects %+v\n%s", run, key, s.creates, s.projects, out)
	}
	got := s.pushed[s.projects[0].ID]
	if len(got) == 0 {
		t.Fatalf("%s: project %s got no push\n%s", run, key, out)
	}
	for _, c := range got {
		if c != content {
			t.Fatalf("%s: project %s got %q, want only %q\n%s", run, key, c, content, out)
		}
	}
}

// Two repositories routed by path globs to two named remotes, with no legacy
// `remote:` block: each lands on its own server under its git-origin key, and
// running the sync again reuses those projects instead of creating new ones.
func TestRemoteSyncAll_RoutesEachProjectByNamedRemotes(t *testing.T) {
	f := newSyncFixture(t)
	work := newFakeRemote(t, "work")
	personal := newFakeRemote(t, "personal")
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"work":     {ServerURL: work.URL, APIKey: "<REDACTED>", Paths: []string{filepath.Join(f.base, "work") + "/*"}},
		"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision, f.notesDir: notesDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--config", cfgPath)
	if code != 0 || strings.Contains(out, "flag provided but not defined") {
		t.Fatalf("remote sync --all exited %d\n%s", code, out)
	}
	wantOnlyProject(t, "first run", work, f.apiKey, apiDecision, out)
	wantOnlyProject(t, "first run", personal, f.notesKey, notesDecision, out)

	out, code = runCLI(t, f.base, "remote", "sync-per-project", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync-per-project exited %d\n%s", code, out)
	}
	wantOnlyProject(t, "second run", work, f.apiKey, apiDecision, out)
	wantOnlyProject(t, "second run", personal, f.notesKey, notesDecision, out)
}

// With one remote, or with --remote, nothing probes the other remotes: the
// second run still finds the project the first created, by its key.
func TestRemoteSyncAll_SecondRunReusesTheProjectWithoutAProbe(t *testing.T) {
	for _, extra := range [][]string{nil, {"--remote", "personal"}} {
		f := newSyncFixture(t)
		personal := newFakeRemote(t, "personal")
		remotes := map[string]config.RemoteEntry{
			"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
		}
		if extra != nil {
			remotes["work"] = config.RemoteEntry{ServerURL: "http://127.0.0.1:1", APIKey: "<REDACTED>"}
		}
		cfgPath := newRemoteTestEnv(t, remotes)
		saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision})

		args := append([]string{"remote", "sync", "--all", "--config", cfgPath}, extra...)
		for run := 1; run <= 2; run++ {
			if out, code := runCLI(t, f.base, args...); code != 0 {
				t.Fatalf("%v run %d exited %d\n%s", extra, run, code, out)
			}
		}
		if s := personal.snapshot(); s.creates != 1 || len(s.pushed[s.projects[0].ID]) != 2 {
			t.Errorf("%v: two runs made %d projects and pushes %v, want one project pushed twice", extra, s.creates, s.pushed)
		}
	}
}

// A repository already registered on a remote without routing paths goes to
// that remote, as `remote sync` sends it, not to the default remote.
func TestRemoteSyncAll_UsesTheRemoteThatKnowsTheRepo(t *testing.T) {
	f := newSyncFixture(t)
	company := newFakeRemote(t, "company")
	company.projects = []remotesync.RemoteProject{{ID: "acme-api", Slug: "api", RemoteKey: f.apiKey}}
	personal := newFakeRemote(t, "personal")
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"company":  {ServerURL: company.URL, APIKey: "<REDACTED>"},
		"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync --all exited %d\n%s", code, out)
	}
	if s := personal.snapshot(); s.creates != 0 || len(s.pushed) != 0 {
		t.Fatalf("acme/api, registered on company, reached personal: %+v\n%s", s, out)
	}
	if got := company.snapshot().pushed["acme-api"]; len(got) != 1 || got[0] != apiDecision {
		t.Fatalf("company project got %v\n%s", got, out)
	}
}

// An explicit glob names the repository's remote: a project another remote
// lists under the same git-origin key does not take the memories. Repo-scoped
// `remote sync` and `remote status` follow the same rule.
func TestRemoteSync_ExplicitGlobWinsOverAKeyRegisteredElsewhere(t *testing.T) {
	f := newSyncFixture(t)
	work := newFakeRemote(t, "work")
	shared := newFakeRemote(t, "shared")
	shared.projects = []remotesync.RemoteProject{{ID: "claimed", Slug: "api", RemoteKey: f.apiKey}}
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"work":   {ServerURL: work.URL, APIKey: "<REDACTED>", Paths: []string{filepath.Join(f.base, "work") + "/*"}},
		"shared": {ServerURL: shared.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision})

	sub := filepath.Join(f.apiDir, "internal", "billing")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{f.apiDir, sub} {
		out, code := runCLI(t, dir, "remote", "sync", "--config", cfgPath)
		if code == 0 || len(shared.snapshot().pushed) != 0 {
			t.Fatalf("remote sync from %s exited %d and pushed %v to shared, want a refusal and nothing on shared\n%s", dir, code, shared.snapshot().pushed, out)
		}
	}

	out, code := runCLI(t, f.apiDir, "remote", "status", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, `registered on "shared"`) {
		t.Errorf("remote status must say the repo is registered on another remote than its glob's (exit %d)\n%s", code, out)
	}

	out, code = runCLI(t, f.base, "remote", "sync", "--all", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync --all exited %d\n%s", code, out)
	}
	if got := shared.snapshot().pushed; len(got) != 0 {
		t.Fatalf("shared, which only claims the key, got %v\n%s", got, out)
	}
	wantOnlyProject(t, "sync --all", work, f.apiKey, apiDecision, out)
}

// When a remote that may own the repository does not answer, its memories
// wait: no project is created for them on another remote.
func TestRemoteSyncAll_HoldsBehindASilentRemote(t *testing.T) {
	f := newSyncFixture(t)
	company := newFakeRemote(t, "company")
	company.Close()
	personal := newFakeRemote(t, "personal")
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"company":  {ServerURL: company.URL, APIKey: "<REDACTED>"},
		"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--config", cfgPath)
	if code != 1 {
		t.Errorf("a run that held a project exited %d, want 1\n%s", code, out)
	}
	if s := personal.snapshot(); s.creates != 0 || len(s.pushed) != 0 {
		t.Fatalf("a repository company may own was sent to personal while company was down: %+v\n%s", s, out)
	}
	if !strings.Contains(out, "[api] held:") {
		t.Errorf("the run must say the project is held:\n%s", out)
	}
}

// With no default remote and no glob for its path, a repository still goes to
// the remote that already knows it.
func TestRemoteSyncAll_FindsAnUnroutedRepoOnTheRemoteThatKnowsIt(t *testing.T) {
	f := newSyncFixture(t)
	company := newFakeRemote(t, "company")
	lab := newFakeRemote(t, "lab")
	lab.projects = []remotesync.RemoteProject{{ID: "lab-notes", Slug: "notes", RemoteKey: f.notesKey}}
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"company": {ServerURL: company.URL, APIKey: "<REDACTED>", Paths: []string{filepath.Join(f.base, "work") + "/*"}},
		"lab":     {ServerURL: lab.URL, APIKey: "<REDACTED>"},
	})
	saveDecisions(t, cfgPath, map[string]string{f.notesDir: notesDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync --all exited %d\n%s", code, out)
	}
	if got := lab.snapshot().pushed["lab-notes"]; len(got) != 1 || got[0] != notesDecision {
		t.Fatalf("lab-notes got %v, want the notes decision\n%s", got, out)
	}
}

func TestRemoteSyncAll_DryRunMakesNoNetworkCalls(t *testing.T) {
	f := newSyncFixture(t)
	work := newFakeRemote(t, "work")
	personal := newFakeRemote(t, "personal")
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"work":     {ServerURL: work.URL, APIKey: "<REDACTED>", Paths: []string{filepath.Join(f.base, "work") + "/*"}},
		"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision, f.notesDir: notesDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--dry-run", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync --all --dry-run exited %d\n%s", code, out)
	}
	if w, p := work.snapshot().requests, personal.snapshot().requests; w != 0 || p != 0 {
		t.Fatalf("a dry run made %d requests to work and %d to personal\n%s", w, p, out)
	}
	for _, line := range []string{"→ work", "→ personal"} {
		if !strings.Contains(out, line) {
			t.Errorf("the plan must show %q:\n%s", line, out)
		}
	}
}

func TestRemoteSyncAll_RemoteFlagSendsEveryProjectThere(t *testing.T) {
	f := newSyncFixture(t)
	work := newFakeRemote(t, "work")
	personal := newFakeRemote(t, "personal")
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"work":     {ServerURL: work.URL, APIKey: "<REDACTED>", Paths: []string{filepath.Join(f.base, "work") + "/*"}},
		"personal": {ServerURL: personal.URL, APIKey: "<REDACTED>", Default: true},
	})
	saveDecisions(t, cfgPath, map[string]string{f.apiDir: apiDecision, f.notesDir: notesDecision})

	out, code := runCLI(t, f.base, "remote", "sync", "--all", "--remote", "personal", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote sync --all --remote personal exited %d\n%s", code, out)
	}
	if s := work.snapshot(); s.requests != 0 {
		t.Fatalf("--remote personal still asked work (%d requests)\n%s", s.requests, out)
	}
	if s := personal.snapshot(); s.creates != 2 || len(s.pushed) != 2 {
		t.Fatalf("personal got %d projects and pushes %v, want both repositories\n%s", s.creates, s.pushed, out)
	}
}

func TestRemoteSyncAll_RejectsFlagsItCannotHonor(t *testing.T) {
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"personal": {ServerURL: "http://127.0.0.1:1", APIKey: "<REDACTED>", Default: true},
	})
	out, code := runCLI(t, t.TempDir(), "remote", "sync", "--all", "--project-id", "x", "--config", cfgPath)
	if code != 2 || !strings.Contains(out, "--all syncs every local project") {
		t.Errorf("--all --project-id exited %d, want 2 with an explanation\n%s", code, out)
	}
	out, code = runCLI(t, t.TempDir(), "remote", "sync", "--all", "--remote", "nowhere", "--config", cfgPath)
	if code != 1 || !strings.Contains(out, `remote "nowhere" not found in config`) {
		t.Errorf("--all --remote nowhere exited %d, want 1 naming the remote\n%s", code, out)
	}

	// The only server is a legacy block turned off: say so, not "configure one".
	disabled := newTestEnvWith(t, func(cfg *config.Config) {
		cfg.Remote.ServerURL, cfg.Remote.APIKey, cfg.Remote.Enabled = "http://127.0.0.1:1", "<REDACTED>", false
	})
	out, code = runCLI(t, t.TempDir(), "remote", "sync", "--all", "--config", disabled)
	if code != 1 || !strings.Contains(out, "Remote sync is disabled") {
		t.Errorf("--all with a disabled remote exited %d, want 1 saying it is disabled\n%s", code, out)
	}
}

// A remote project is matched to a repository by its git-origin keys. A slug
// match is adopted only when no repository owns that project: a project an
// earlier run created under its timestamped key is reused, one registered to
// another repository is left alone. The project list is read once per run.
func TestRemoteProjectFor_MatchesRepositoriesByKeyNotSlug(t *testing.T) {
	remote := newFakeRemote(t, "work")
	remote.projects = []remotesync.RemoteProject{
		{ID: "other-api", Slug: "api", RemoteKey: "key-of-another-repo"},
		{ID: "old-notes", Slug: "notes", RemoteKey: "notes-1696000000"},
		{ID: "other-shared", Slug: "shared", RemoteKey: "key-of-a-shared-repo"},
	}
	entry := config.RemoteEntry{Name: "work", ServerURL: remote.URL, APIKey: "<REDACTED>"}
	target := &perProjectTarget{
		client: remotesync.NewClientFromEntry(entry, "test"),
		admin:  newAdminAPI(entry.ServerURL, entry.APIKey),
	}
	ctx := context.Background()

	// Same folder name as another repository's project: created beside it.
	id, key, err := remoteProjectFor(ctx, target, "api", "api", []string{"key-of-this-repo"})
	if err != nil || id == "other-api" || key != "key-of-this-repo" {
		t.Fatalf("remoteProjectFor(api) = %q, %q, %v; want a project of its own", id, key, err)
	}

	id, key, err = remoteProjectFor(ctx, target, "notes", "notes", []string{"key-of-notes"})
	if err != nil || id != "old-notes" || key != "key-of-notes" {
		t.Fatalf("remoteProjectFor(notes) = %q, %q, %v; want the project an earlier run created", id, key, err)
	}

	// No origin to tell the two apart: refuse instead of adopting.
	if id, _, err := remoteProjectFor(ctx, target, "shared", "shared", nil); err == nil {
		t.Fatalf("a project without an origin adopted another repository's project %q by slug", id)
	}

	id, key, err = remoteProjectFor(ctx, target, "misc", "misc", nil)
	if err != nil || key != "" {
		t.Fatalf("remoteProjectFor(misc) = %q, %q, %v", id, key, err)
	}
	again, _, err := remoteProjectFor(ctx, target, "misc", "misc", nil)
	if err != nil || again != id {
		t.Fatalf("second lookup for a project without an origin = %q, %v; want %q", again, err, id)
	}

	s := remote.snapshot()
	if s.creates != 2 || s.lists != 1 {
		t.Errorf("creates = %d, lists = %d; want 2 (api, misc) and 1", s.creates, s.lists)
	}
	for _, p := range s.projects {
		if p.ID == id && p.RemoteKey != localProjectKey("misc") {
			t.Errorf("misc was created under %q, want %q", p.RemoteKey, localProjectKey("misc"))
		}
		if p.RemoteKey == "key-of-this-repo" && p.Slug == "api" {
			t.Errorf("the second api took the taken slug: %+v", p)
		}
	}
}

// A remote that knows the repository only under its v1 key gets the memories
// stamped with that key, as repo-scoped sync stamps them.
func TestRemoteProjectFor_ReturnsTheKeyTheRemoteKnows(t *testing.T) {
	remote := newFakeRemote(t, "work")
	remote.projects = []remotesync.RemoteProject{{ID: "api-v1", Slug: "api", RemoteKey: "v1-key"}}
	entry := config.RemoteEntry{Name: "work", ServerURL: remote.URL, APIKey: "<REDACTED>"}
	target := &perProjectTarget{
		client: remotesync.NewClientFromEntry(entry, "test"),
		admin:  newAdminAPI(entry.ServerURL, entry.APIKey),
	}
	id, key, err := remoteProjectFor(context.Background(), target, "api", "api", []string{"canonical-key", "v1-key"})
	if err != nil || id != "api-v1" || key != "v1-key" {
		t.Fatalf("remoteProjectFor = %q, %q, %v; want api-v1 under v1-key", id, key, err)
	}
}

// Each project's memories are checked against that project's own root: its
// paths are rewritten relative to it, and a memory without a project keeps
// every local path blocked, wherever the command runs from.
func TestPlanPerProject_ChecksEachProjectAgainstItsOwnRoot(t *testing.T) {
	pid := func(s string) *string { return &s }
	memories := []remotesync.Memory{
		{ID: "m1", Category: "decision", ProjectID: pid("p-api"),
			Content: "Decided that the upload retry policy lives in /home/dev/work/api/internal/retry.go and every client reads it from there"},
		{ID: "m2", Category: "decision",
			Content: "Decided that personal notes about the release checklist are kept in /home/dev/notes/todo.md and never shared anywhere"},
		{ID: "m3", Category: "decision", ProjectID: pid("p-b"),
			Content: "Decided that the second api clone keeps its own build cache so both checkouts can run tests at the same time safely"},
		{ID: "m4", Category: "decision", ProjectID: pid("p-a"),
			Content: "Decided that the first api clone keeps its own build cache so both checkouts can run tests at the same time safely"},
	}
	locals := map[string]localProject{
		"p-api": {name: "api", path: "/home/dev/work/api"},
		"p-a":   {name: "clone", path: "/home/dev/a/clone"},
		"p-b":   {name: "clone", path: "/home/dev/b/clone"},
	}

	buckets := planPerProject(memories, locals, "misc")
	var names []string
	for _, b := range buckets {
		names = append(names, b.key)
		if b.key == "" {
			t.Errorf("memories without a project got a bucket with a local path in them: %+v", b.memories)
		}
		if b.key == "p-api" {
			if len(b.memories) != 1 || !strings.Contains(b.memories[0].Content, "./internal/retry.go") || strings.Contains(b.memories[0].Content, "/home/dev") {
				t.Errorf("api memory not rewritten against its own root: %+v", b.memories)
			}
		}
	}
	if got := strings.Join(names, ","); got != "p-api,p-a,p-b" {
		t.Errorf("bucket order = %s, want p-api,p-a,p-b (largest, then name, then id)", got)
	}
}

// The key the checkout derives now outranks a stored one that went stale, which
// stays as a fallback.
func TestLocalProjectKeys_PrefersTheCheckoutsKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "api")
	key := gitRepo(t, dir, "https://github.com/acme/api.git")
	got := localProjectKeys(localProject{name: "api", path: dir, remoteKey: "stale-key"})
	if len(got) < 2 || got[0] != key || got[1] != "stale-key" {
		t.Fatalf("localProjectKeys = %v, want [%s stale-key ...]", got, key)
	}
	if got := localProjectKeys(localProject{name: "gone", path: filepath.Join(dir, "missing"), remoteKey: "stored-key"}); len(got) != 1 || got[0] != "stored-key" {
		t.Fatalf("a checkout that is gone keeps its stored key, got %v", got)
	}
}

// A memory whose project is missing from the local store is labelled from its
// project id, however short.
func TestLocalProjectLabel_ShortIDs(t *testing.T) {
	for _, c := range []struct{ pid, name, want string }{
		{"p1", "", "project-p1"},
		{"", "", "project-"},
		{"0123456789abcdef", "", "project-01234567"},
		{"p1", "api", "api"},
	} {
		if got := localProjectLabel(c.pid, c.name); got != c.want {
			t.Errorf("localProjectLabel(%q, %q) = %q, want %q", c.pid, c.name, got, c.want)
		}
	}
}
