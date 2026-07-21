package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	projectpkg "github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/sync"
)

// TestResolveLinkedTarget locks the linked-project routing guard:
//   - inside a known local project, the push is bounded, so it forces the
//     linked project id without requiring --full-store.
//   - outside any repo, the whole store would be pushed, so it refuses unless
//     --full-store is given, and the error guides toward the common fixes.
func TestResolveLinkedTarget(t *testing.T) {
	const linked = "c035006e-1f49-4e56-8b0c-3eb8056308b0"

	// In a known local project: always allowed, --full-store irrelevant.
	proj := &projectpkg.Project{ID: "local1", Name: "repo"}
	for _, full := range []bool{false, true} {
		got, err := resolveLinkedTarget(proj, linked, full)
		if err != nil {
			t.Fatalf("proj set, fullStore=%v: unexpected error: %v", full, err)
		}
		if got != linked {
			t.Fatalf("proj set: want %q, got %q", linked, got)
		}
	}

	// No local project, no --full-store: refuse with guidance.
	_, err := resolveLinkedTarget(nil, linked, false)
	if err == nil {
		t.Fatal("expected refusal when cwd is not a known project and --full-store is absent")
	}
	msg := err.Error()
	if !strings.Contains(msg, linked) {
		t.Errorf("error should name the linked project, got %q", msg)
	}
	if !strings.Contains(msg, "--full-store") {
		t.Errorf("error should point to --full-store, got %q", msg)
	}
	if !strings.Contains(msg, "git rev-parse") {
		t.Errorf("error should flag the git-not-detected case, got %q", msg)
	}

	// No local project, but --full-store given: allowed (push entire store).
	got, err := resolveLinkedTarget(nil, linked, true)
	if err != nil {
		t.Fatalf("fullStore should allow the push: %v", err)
	}
	if got != linked {
		t.Fatalf("want %q, got %q", linked, got)
	}
}

func slugProjectsServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"id":"11111111-2222-3333-4444-555555555555","name":"API","slug":"api","remote_key":"k1"},
			{"id":"66666666-7777-8888-9999-aaaaaaaaaaaa","name":"Web","slug":"web","remote_key":"k2"}
		]`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestResolveLinkTargetUUIDPassthrough(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	got, err := resolveLinkTarget(id, config.RemoteEntry{ServerURL: "http://unused.example.com"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != id {
		t.Fatalf("UUID should pass through unchanged, got %q", got)
	}
}

func TestResolveLinkTargetSlugMatch(t *testing.T) {
	ts := slugProjectsServer(t)
	got, err := resolveLinkTarget("web", config.RemoteEntry{ServerURL: ts.URL, APIKey: "k"}, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "66666666-7777-8888-9999-aaaaaaaaaaaa" {
		t.Fatalf("slug 'web' should resolve to its project id, got %q", got)
	}
}

func TestResolveLinkTargetSlugNotFound(t *testing.T) {
	ts := slugProjectsServer(t)
	_, err := resolveLinkTarget("nope", config.RemoteEntry{ServerURL: ts.URL, APIKey: "k"}, "company")
	if err == nil {
		t.Fatal("expected an error for an unknown slug")
	}
	msg := err.Error()
	if !strings.Contains(msg, `no project with slug "nope" on remote "company"`) {
		t.Errorf("error message missing slug/remote context: %q", msg)
	}
	// Available slugs should be listed to guide the user.
	if !strings.Contains(msg, "api") || !strings.Contains(msg, "web") {
		t.Errorf("error message should list available slugs, got %q", msg)
	}
}

// TestRemoteKeysMatch locks the push-target guard: a forced/linked project
// only matches when one of the repo's derived keys equals one of the remote
// project's routing keys; empty keys never match.
func TestRemoteKeysMatch(t *testing.T) {
	rp := &sync.RemoteProject{RemoteKey: "canon123", RemoteKeyV1: "legacy456"}
	if !remoteKeysMatch(rp, "canon123", "") {
		t.Fatal("canonical key should match remote_key")
	}
	if !remoteKeysMatch(rp, "nope", "legacy456") {
		t.Fatal("legacy key should match remote_key_v1")
	}
	if remoteKeysMatch(rp, "other", "another") {
		t.Fatal("disjoint keys must not match")
	}
	if remoteKeysMatch(rp, "", "") {
		t.Fatal("empty keys must never match")
	}
}
