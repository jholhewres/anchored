package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// TestSaveRemote_RunsTheClassifier locks `anchored save --remote` to the
// auto-sync classifier, without its quality gates: a memory saved outside a
// repository (user scope) or carrying a secret stays local with a warning
// that names the reason, and the remote is not even contacted; a project
// decision is sent, a short one too, with a path in the project made
// relative.
func TestSaveRemote_RunsTheClassifier(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	key := gitRepo(t, repo, "https://github.com/acme/classifier.git")
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(base, "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	remote := newFakeRemote(t, "team")
	remote.projects = []remotesync.RemoteProject{{ID: "rp-1", Slug: "classifier", RemoteKey: key}}
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"default": {ServerURL: remote.URL, APIKey: "<REDACTED>", Default: true, Projects: []string{"rp-1"}},
	})

	for _, tc := range []struct {
		dir, content, category, reason string
	}{
		{scratch, "personal note: the staging cluster restarts every night at three", "fact", "user_scope_blocked"},
		{repo, "the deploy bot authenticates with ghp_" + strings.Repeat("a", 36), "decision", "secret_pattern"},
	} {
		before := remote.snapshot().requests
		out, code := runCLI(t, tc.dir, "save", tc.content, "--category", tc.category, "--remote=default", "--config", cfgPath)
		if code != 0 || !strings.Contains(out, "blocked by the safety policy ("+tc.reason+")") {
			t.Errorf("save %q exited %d, want a warning naming %s\n%s", tc.content, code, tc.reason, out)
		}
		if n := remote.snapshot().requests - before; n != 0 {
			t.Errorf("save %q made %d requests to the remote it blocked", tc.content, n)
		}
	}

	const short = "Settled on Postgres for the queue"
	inRepo := "Decided that the queue reads " + filepath.Join(realRepo, "config", "queue.yaml") + " at startup"
	for _, content := range []string{short, inRepo} {
		out, code := runCLI(t, repo, "save", content, "--category", "decision", "--remote=default", "--config", cfgPath)
		if code != 0 || !strings.Contains(out, "Saved to remote default") {
			t.Fatalf("project decision %q: exited %d, want it sent\n%s", content, code, out)
		}
	}
	got := remote.snapshot().pushed["rp-1"]
	want := []string{short, "Decided that the queue reads ./config/queue.yaml at startup"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("the remote received %q, want %q", got, want)
	}

	if out, code := runCLI(t, repo, "save", "a note about the dentist", "--category", "note", "--config", cfgPath); code == 0 || !strings.Contains(out, "unknown category") {
		t.Fatalf("save --category note exited %d, want a refusal\n%s", code, out)
	}
}
