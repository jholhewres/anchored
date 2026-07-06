package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallPluginFromMirror_HappyPath copies a mock mirror tree into the
// cache dir, updates a synthetic registry, and confirms (a) the cache holds
// the new version's files, (b) the registry entry for anchored points at the
// new install path, (c) other plugins' entries are untouched.
func TestInstallPluginFromMirror_HappyPath(t *testing.T) {
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")

	// Seed mirror with a minimal plugin tree.
	seedMirrorManifest(t, mirror, "0.4.9")
	writeFile(t, filepath.Join(mirror, "hooks", "hooks.json"), `{"hooks":{}}`)
	writeFile(t, filepath.Join(mirror, "README.md"), "# mirror v0.4.9")
	// .git is excluded from the copy.
	writeFile(t, filepath.Join(mirror, ".git", "HEAD"), "ref: refs/heads/main\n")

	// Seed registry with another plugin so we can prove non-interference.
	registry := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"other-plugin@somewhere": []map[string]any{{
				"scope":       "user",
				"installPath": "/some/other/path",
				"version":     "1.2.3",
				"installedAt": "2025-01-01T00:00:00Z",
				"lastUpdated": "2025-01-01T00:00:00Z",
			}},
		},
	}
	writeJSON(t, registryPath, registry)

	if err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9"); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Cache holds the new version's files (but not the .git tree).
	finalDir := filepath.Join(cacheParent, "0.4.9")
	for _, want := range []string{"hooks/hooks.json", "README.md", ".claude-plugin/plugin.json"} {
		if _, err := os.Stat(filepath.Join(finalDir, want)); err != nil {
			t.Errorf("expected %s in cache, stat err=%v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(finalDir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git should NOT be copied into cache, stat err=%v", err)
	}

	// Registry updated for anchored.
	var doc map[string]any
	readJSON(t, registryPath, &doc)
	plugins := doc["plugins"].(map[string]any)
	anchoredEntries := plugins["anchored@anchored"].([]any)
	if len(anchoredEntries) != 1 {
		t.Fatalf("want 1 anchored entry, got %d", len(anchoredEntries))
	}
	entry := anchoredEntries[0].(map[string]any)
	if entry["version"] != "0.4.9" {
		t.Errorf("entry version = %v, want 0.4.9", entry["version"])
	}
	if entry["installPath"] != finalDir {
		t.Errorf("entry installPath = %v, want %s", entry["installPath"], finalDir)
	}

	// Other plugin's entry MUST be preserved verbatim.
	otherEntries := plugins["other-plugin@somewhere"].([]any)
	otherEntry := otherEntries[0].(map[string]any)
	if otherEntry["installPath"] != "/some/other/path" || otherEntry["version"] != "1.2.3" {
		t.Errorf("other plugin entry was clobbered: %+v", otherEntry)
	}

	// Schema version preserved.
	if v, _ := doc["version"].(float64); int(v) != supportedRegistrySchema {
		t.Errorf("schema version changed: got %v, want %d", doc["version"], supportedRegistrySchema)
	}
}

// TestInstallPluginFromMirror_RefusesUnsupportedSchema is the critical safety
// guard: if Claude Code bumps installed_plugins.json to a schema we don't
// understand, anchored MUST abort before touching anything. The cache stays
// untouched and the registry is bit-for-bit identical.
func TestInstallPluginFromMirror_RefusesUnsupportedSchema(t *testing.T) {
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.9")

	registry := map[string]any{
		"version": 99, // Future schema
		"plugins": map[string]any{},
	}
	writeJSON(t, registryPath, registry)
	registryBefore := readFile(t, registryPath)

	err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9")
	if err == nil {
		t.Fatal("expected schema rejection, got nil")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Errorf("expected schema mismatch error, got %v", err)
	}

	// Registry untouched.
	if readFile(t, registryPath) != registryBefore {
		t.Error("registry was modified after schema rejection")
	}
	// Cache untouched.
	if _, err := os.Stat(filepath.Join(cacheParent, "0.4.9")); !os.IsNotExist(err) {
		t.Errorf("cache should not be created after schema rejection")
	}
}

// TestInstallPluginFromMirror_PreservesInstalledAt confirms the timeline
// field is inherited from any existing entry so the user's "first installed"
// date doesn't reset on every auto-update.
func TestInstallPluginFromMirror_PreservesInstalledAt(t *testing.T) {
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.9")

	registry := map[string]any{
		"version": 2,
		"plugins": map[string]any{
			"anchored@anchored": []map[string]any{{
				"scope":       "user",
				"installPath": "/old/path",
				"version":     "0.4.0",
				"installedAt": "2025-12-01T00:00:00Z",
				"lastUpdated": "2025-12-01T00:00:00Z",
			}},
		},
	}
	writeJSON(t, registryPath, registry)

	if err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9"); err != nil {
		t.Fatalf("install: %v", err)
	}

	var doc map[string]any
	readJSON(t, registryPath, &doc)
	entry := doc["plugins"].(map[string]any)["anchored@anchored"].([]any)[0].(map[string]any)
	if entry["installedAt"] != "2025-12-01T00:00:00Z" {
		t.Errorf("installedAt should be inherited, got %v", entry["installedAt"])
	}
	if entry["lastUpdated"] == "2025-12-01T00:00:00Z" {
		t.Errorf("lastUpdated should be refreshed, but matches old installedAt")
	}
}

// TestInstallPluginFromMirror_AtomicSwap confirms that when a cache dir
// already exists at the target version, it's replaced atomically (no
// half-written state visible mid-install).
func TestInstallPluginFromMirror_AtomicSwap(t *testing.T) {
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.9")
	writeFile(t, filepath.Join(mirror, "marker"), "NEW")

	// Pre-existing cache entry with stale content.
	existing := filepath.Join(cacheParent, "0.4.9")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(existing, "marker"), "OLD")
	writeFile(t, filepath.Join(existing, "should-be-gone"), "stale")

	writeJSON(t, registryPath, map[string]any{"version": 2, "plugins": map[string]any{}})
	if err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9"); err != nil {
		t.Fatalf("install: %v", err)
	}
	got := readFile(t, filepath.Join(existing, "marker"))
	if got != "NEW" {
		t.Errorf("marker = %q, want NEW (atomic swap failed)", got)
	}
	if _, err := os.Stat(filepath.Join(existing, "should-be-gone")); !os.IsNotExist(err) {
		t.Error("stale file from prior install leaked into new cache")
	}
}

// TestInstallPluginFromMirror_RewritesHookCommandsToAbsolutePath confirms
// that installing patches the installed copy's hooks/hooks.json commands to
// the absolute binary path, without disturbing the JSON structure or
// unrelated fields.
func TestInstallPluginFromMirror_RewritesHookCommandsToAbsolutePath(t *testing.T) {
	withFakeBinaryPath(t)
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.9")

	writeFile(t, filepath.Join(mirror, "hooks", "hooks.json"), `{
  "description": "test hooks",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {"type": "command", "command": "anchored hook sessionstart"}
        ]
      }
    ],
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {"type": "command", "command": "anchored hook stop"}
        ]
      }
    ]
  }
}`)
	writeJSON(t, registryPath, map[string]any{"version": 2, "plugins": map[string]any{}})

	if err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9"); err != nil {
		t.Fatalf("install: %v", err)
	}

	var doc map[string]any
	readJSON(t, filepath.Join(cacheParent, "0.4.9", "hooks", "hooks.json"), &doc)

	if doc["description"] != "test hooks" {
		t.Errorf("unrelated field lost: %v", doc["description"])
	}

	hooks := doc["hooks"].(map[string]any)
	sessionStartCmd := hooks["SessionStart"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	if sessionStartCmd != fakeBinaryPath+" hook sessionstart" {
		t.Errorf("SessionStart command = %v, want %q", sessionStartCmd, fakeBinaryPath+" hook sessionstart")
	}
	stopCmd := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]
	if stopCmd != fakeBinaryPath+" hook stop" {
		t.Errorf("Stop command = %v, want %q", stopCmd, fakeBinaryPath+" hook stop")
	}
}

// TestInstallPluginFromMirror_MissingHooksJSONIsNonFatal confirms the
// install still succeeds when hooks/hooks.json isn't present in the mirror
// (the rewrite step must be a silent no-op, not an install-breaking error).
func TestInstallPluginFromMirror_MissingHooksJSONIsNonFatal(t *testing.T) {
	withFakeBinaryPath(t)
	mirror := t.TempDir()
	cacheParent := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.9")
	writeJSON(t, registryPath, map[string]any{"version": 2, "plugins": map[string]any{}})

	if err := installPluginFromMirror(mirror, cacheParent, registryPath, "0.4.9"); err != nil {
		t.Fatalf("install should succeed without hooks.json: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(raw))
}

func readJSON(t *testing.T, path string, dst any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
