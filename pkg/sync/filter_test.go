package sync

import (
	"strings"
	"testing"
)

func TestRemoteSafetyFilter_LocalPathLinux(t *testing.T) {
	result := RemoteSafetyFilter(
		"project lives at /home/alice/projects/myapp",
		nil, "",
	)
	if result.Allowed {
		t.Error("expected Blocked=true for Linux home path")
	}
	if !result.Blocked {
		t.Error("expected Blocked=true")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "local_path" && strings.Contains(v.Value, "/home/alice") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected local_path violation for /home/alice, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_LocalPathMacOS(t *testing.T) {
	result := RemoteSafetyFilter(
		"code at /Users/bob/dev/project",
		nil, "",
	)
	if result.Allowed {
		t.Error("expected Blocked for macOS home path")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "local_path" && strings.Contains(v.Value, "/Users/bob") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected local_path violation for /Users/bob, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_LocalPathWindows(t *testing.T) {
	result := RemoteSafetyFilter(
		`project at C:\Users\carol\code\app`,
		nil, "",
	)
	if result.Allowed {
		t.Error("expected Blocked for Windows home path")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "local_path" && strings.Contains(v.Value, "carol") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected local_path violation for Windows path, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_TildePath(t *testing.T) {
	result := RemoteSafetyFilter(
		"config at ~/projects/myapp",
		nil, "",
	)
	if result.Allowed {
		t.Error("expected Blocked for tilde path")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "local_path" && strings.Contains(v.Value, "~/") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected local_path violation for tilde path, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_TempPaths(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"tmp", "/tmp/build-output.log"},
		{"var_folders", "/var/folders/xy/abc123/T/build"},
		{"private_tmp", "/private/tmp/session-data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := RemoteSafetyFilter(
				"file at "+tc.path,
				nil, "",
			)
			if result.Allowed {
				t.Errorf("expected Blocked for temp path %s", tc.path)
			}
			found := false
			for _, v := range result.Violations {
				if v.Reason == "local_path" {
					found = true
				}
			}
			if !found {
				t.Errorf("expected local_path violation for %s, got: %+v", tc.path, result.Violations)
			}
		})
	}
}

func TestRemoteSafetyFilter_ProjectRelativeRewrite(t *testing.T) {
	result := RemoteSafetyFilter(
		"edit /home/alice/projects/myapp/src/main.go and /home/alice/projects/myapp/README.md",
		nil,
		"/home/alice/projects/myapp",
	)

	for _, v := range result.Violations {
		if v.Reason == "local_path" && strings.Contains(v.Value, "/home/alice/projects/myapp") {
			t.Errorf("project-root paths should not be flagged as violations, got: %+v", v)
		}
	}

	if !strings.Contains(result.Content, "./src/main.go") {
		t.Errorf("expected project-root path rewritten to relative, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "./README.md") {
		t.Errorf("expected README.md path rewritten, got: %s", result.Content)
	}
}

func TestRemoteSafetyFilter_SecretDetection(t *testing.T) {
	result := RemoteSafetyFilter(
		"api_key=sk-abcdefghijklmnopqrstuvwxyz12345 for the service",
		nil, "",
	)
	if result.Allowed {
		t.Error("expected Blocked for secret")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "secret_pattern" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected secret_pattern violation, got: %+v", result.Violations)
	}
	if !strings.Contains(result.Content, "[REDACTED]") {
		t.Errorf("expected content to be sanitized, got: %s", result.Content)
	}
}

func TestRemoteSafetyFilter_KnownTokenPrefixes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{name: "github pat", content: "token ghp_ABCDEFGHIJKLMNOP should never sync"},
		{name: "slack bot", content: "bot xoxb-1234567890-abcdef should never sync"},
		{name: "credential uri", content: "postgres://user:pass@example.com/db should never sync"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := RemoteSafetyFilter(tt.content, nil, "")
			if !result.Blocked {
				t.Fatalf("expected known token prefix to be blocked")
			}
			if result.Allowed {
				t.Fatalf("expected known token prefix to be disallowed")
			}
			if len(result.Violations) == 0 || result.Violations[0].Reason != "secret_pattern" {
				t.Fatalf("expected secret_pattern violation, got %+v", result.Violations)
			}
		})
	}
}

func TestRemoteSafetyFilter_PersonalPreference(t *testing.T) {
	result := RemoteSafetyFilter(
		"prefer dark theme",
		map[string]any{"scope": "user"},
		"",
	)
	if result.Allowed {
		t.Error("expected Blocked for personal preference")
	}
	found := false
	for _, v := range result.Violations {
		if v.Reason == "personal_preference" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected personal_preference violation, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_ProjectScopeAllowed(t *testing.T) {
	result := RemoteSafetyFilter(
		"uses PostgreSQL for main database",
		map[string]any{"scope": "project"},
		"",
	)
	if !result.Allowed {
		t.Errorf("project-scoped safe content should be allowed, violations: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_CleanContentPasses(t *testing.T) {
	result := RemoteSafetyFilter(
		"the application uses React for the frontend",
		map[string]any{"scope": "project"},
		"",
	)
	if !result.Allowed {
		t.Error("clean content should pass")
	}
	if result.Blocked {
		t.Error("clean content should not be blocked")
	}
	if len(result.Violations) != 0 {
		t.Errorf("expected no violations, got: %+v", result.Violations)
	}
}

func TestRemoteSafetyFilter_NilMetadata(t *testing.T) {
	result := RemoteSafetyFilter(
		"simple memory with no secrets",
		nil,
		"",
	)
	if !result.Allowed {
		t.Error("clean content with nil metadata should pass")
	}
}

func TestRemoteSafetyFilter_Truncation(t *testing.T) {
	longPath := "/home/user/" + strings.Repeat("a", 100) + "/file.txt"
	result := RemoteSafetyFilter(longPath, nil, "")
	for _, v := range result.Violations {
		if len(v.Value) > 53 {
			t.Errorf("violation value should be truncated to ~50 chars, got %d: %s", len(v.Value), v.Value)
		}
	}
}

func TestRemoteSafetyFilter_NoHomePathInCleanContent(t *testing.T) {
	result := RemoteSafetyFilter(
		"the config file is at ./config/settings.yaml",
		nil, "",
	)
	if !result.Allowed {
		t.Errorf("relative paths should be allowed, violations: %+v", result.Violations)
	}
}

// TestSafeTriplesForRemote locks the safety filter on knowledge-graph
// triples: a secret or a local path outside the project in any field keeps
// the whole triple local, a path inside the project goes out relative to it,
// and a clean triple goes out as it is.
func TestSafeTriplesForRemote(t *testing.T) {
	root := "/home/dev/proj"
	in := []SyncTriple{
		{Subject: "api", Predicate: "uses", Object: "postgres", Confidence: 0.9},
		{Subject: "staging-db", Predicate: "connects_with", Object: "postgres://app:not-a-real-pass@10.0.0.5/app"},
		{Subject: "ci", Predicate: "authenticates_with", Object: "ghp_" + strings.Repeat("a", 36)},
		{Subject: "deploy", Predicate: "reads", Object: "/home/someone/secrets/db.env"},
		{Subject: "/Users/dev/notes", Predicate: "describes", Object: "api"},
		{Subject: "build", Predicate: "writes", Object: "/home/dev/proj/bin/anchored"},
	}
	safe, withheld := SafeTriplesForRemote(in, root)
	if withheld != 4 {
		t.Fatalf("withheld %d triples, want 4; sent %+v", withheld, safe)
	}
	want := []SyncTriple{
		{Subject: "api", Predicate: "uses", Object: "postgres", Confidence: 0.9},
		{Subject: "build", Predicate: "writes", Object: "./bin/anchored"},
	}
	if len(safe) != len(want) {
		t.Fatalf("sent %+v, want %+v", safe, want)
	}
	for i := range want {
		if safe[i] != want[i] {
			t.Fatalf("triple %d sent as %+v, want %+v", i, safe[i], want[i])
		}
	}
}

// TestRemoteSafetyFilter_ProjectRootIsAPathBoundary locks the project root as
// a whole path segment (attack h5 of 22/11: /home/dev/proj-private passed as
// inside /home/dev/proj). A sibling that only shares the root's prefix stays
// a blocked local path and is not rewritten, a path inside the root is sent
// relative to it, a trailing separator on the root changes nothing, and the
// filesystem root holds no project.
func TestRemoteSafetyFilter_ProjectRootIsAPathBoundary(t *testing.T) {
	const root = "/home/dev/proj"
	if r := RemoteSafetyFilter("notes in /home/dev/proj-private/plan.txt", nil, root); !r.Blocked {
		t.Fatalf("a path beside the project root passed as inside it and syncs as %q", r.Content)
	}
	for _, tc := range []struct{ content, root, want string }{
		{"config at /home/dev/proj/config.yaml", root, "config at ./config.yaml"},
		{"config at /home/dev/proj/config.yaml", root + "/", "config at ./config.yaml"},
		{"the repository is /home/dev/proj.", root, "the repository is .."},
		{"(see /home/dev/proj)", root, "(see .)"},
		{"build in /srv/proj-old/bin, deploy from /srv/proj/bin", "/srv/proj", "build in /srv/proj-old/bin, deploy from ./bin"},
		{`open C:\Users\dev\proj\main.go`, `C:\Users\dev\proj`, `open .\main.go`},
	} {
		r := RemoteSafetyFilter(tc.content, nil, tc.root)
		if r.Blocked || r.Content != tc.want {
			t.Errorf("RemoteSafetyFilter(%q, root %q) = %q blocked=%v, want %q", tc.content, tc.root, r.Content, r.Blocked, tc.want)
		}
	}
	if r := RemoteSafetyFilter("notes in /home/dev/proj/plan.txt", nil, "/"); !r.Blocked {
		t.Fatalf("the filesystem root as project root let a home path through as %q", r.Content)
	}
	if safe, withheld := SafeTriplesForRemote([]SyncTriple{{Subject: "plan", Predicate: "lives_in", Object: "/home/dev/proj-private/plan.txt"}}, root); withheld != 1 {
		t.Fatalf("a triple naming a sibling of the project root was sent as %+v", safe)
	}
}

// TestRemoteSafetyFilter_TraversalOutOfTheRootIsALocalPath locks the cleaning
// before the boundary check: a path that starts inside the project and climbs
// out of it with .. is a local path outside the project.
func TestRemoteSafetyFilter_TraversalOutOfTheRootIsALocalPath(t *testing.T) {
	if r := RemoteSafetyFilter("read /home/dev/proj/../secrets/db.env", nil, "/home/dev/proj"); !r.Blocked {
		t.Fatalf("a path climbing out of the project passed as inside it and syncs as %q", r.Content)
	}
	if r := RemoteSafetyFilter("see /home/dev/proj/docs/../README.md", nil, "/home/dev/proj"); r.Blocked {
		t.Fatalf("a path that stays inside the project was blocked: %+v", r.Violations)
	}
}
