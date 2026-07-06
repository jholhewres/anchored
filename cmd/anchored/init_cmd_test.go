package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBinaryPath is a deterministic stand-in for the real running
// executable's path, so tests can assert on the exact command written
// instead of whatever path `go test` happens to run from.
const fakeBinaryPath = "/opt/anchored/bin/anchored"

// withFakeBinaryPath overrides osExecutable so anchoredBinaryPath() returns
// fakeBinaryPath for the duration of the test.
func withFakeBinaryPath(t *testing.T) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return fakeBinaryPath, nil }
	t.Cleanup(func() { osExecutable = orig })
}

// --- anchoredBinaryPath ---

func TestAnchoredBinaryPath_UsesRunningExecutable(t *testing.T) {
	withFakeBinaryPath(t)
	// fakeBinaryPath doesn't exist on disk, so EvalSymlinks fails and
	// anchoredBinaryPath falls back to the unresolved exe path — which is
	// exactly fakeBinaryPath here.
	if got := anchoredBinaryPath(); got != fakeBinaryPath {
		t.Errorf("anchoredBinaryPath() = %q, want %q", got, fakeBinaryPath)
	}
}

func TestAnchoredBinaryPath_FallbackToInstallDir(t *testing.T) {
	orig := osExecutable
	osExecutable = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { osExecutable = orig })

	home := t.TempDir()
	t.Setenv("HOME", home)
	installPath := filepath.Join(home, ".anchored", "bin", "anchored")
	writeFile(t, installPath, "binary")

	if got := anchoredBinaryPath(); got != installPath {
		t.Errorf("anchoredBinaryPath() = %q, want %q", got, installPath)
	}
}

func TestAnchoredBinaryPath_FallbackToBare(t *testing.T) {
	orig := osExecutable
	osExecutable = func() (string, error) { return "", os.ErrNotExist }
	t.Cleanup(func() { osExecutable = orig })

	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.anchored/bin/anchored present

	if got := anchoredBinaryPath(); got != "anchored" {
		t.Errorf("anchoredBinaryPath() = %q, want %q", got, "anchored")
	}
}

// --- registerMCPJSON: generic mcpServers tools (e.g. claude-code) ---

func TestRegisterMCPJSON_FreshWritesAbsolutePath(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	var cfg map[string]any
	readJSON(t, filepath.Join(home, ".claude.json"), &cfg)
	entry := cfg["mcpServers"].(map[string]any)["anchored"].(map[string]any)
	if entry["command"] != fakeBinaryPath {
		t.Errorf("command = %v, want %v", entry["command"], fakeBinaryPath)
	}
}

func TestRegisterMCPJSON_PreservesForeignEntryOnFreshAdd(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other-tool": map[string]any{"command": "/usr/bin/other", "args": []string{"--flag"}},
		},
	})

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	var cfg map[string]any
	readJSON(t, path, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	other := servers["other-tool"].(map[string]any)
	if other["command"] != "/usr/bin/other" {
		t.Errorf("foreign entry mutated: %v", other)
	}
	anchored := servers["anchored"].(map[string]any)
	if anchored["command"] != fakeBinaryPath {
		t.Errorf("anchored command = %v, want %v", anchored["command"], fakeBinaryPath)
	}
}

func TestRegisterMCPJSON_RepairsBareCommand(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"anchored": map[string]any{
				"command": "anchored",
				"env":     map[string]any{"FOO": "bar"},
			},
			"other-tool": map[string]any{"command": "/usr/bin/other"},
		},
	})

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	var cfg map[string]any
	readJSON(t, path, &cfg)
	servers := cfg["mcpServers"].(map[string]any)

	anchored := servers["anchored"].(map[string]any)
	if anchored["command"] != fakeBinaryPath {
		t.Errorf("command = %v, want %v", anchored["command"], fakeBinaryPath)
	}
	env, ok := anchored["env"].(map[string]any)
	if !ok || env["FOO"] != "bar" {
		t.Errorf("env field not preserved during repair: %v", anchored)
	}

	other := servers["other-tool"].(map[string]any)
	if other["command"] != "/usr/bin/other" {
		t.Errorf("foreign entry mutated during repair: %v", other)
	}

	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("expected .bak backup, stat err=%v", err)
	}
}

func TestRegisterMCPJSON_SkipsAlreadyAbsoluteCommand(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"anchored": map[string]any{"command": "/custom/path/to/anchored"},
		},
	})
	before := readFile(t, path)

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	after := readFile(t, path)
	if before != after {
		t.Errorf("file changed for an already-absolute custom command:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("unexpected .bak written when skipping an already-absolute command")
	}
}

func TestRegisterMCPJSON_IdempotentSecondRun(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP (1st): %v", err)
	}
	first := readFile(t, path)

	if err := registerMCP("claude-code", "."); err != nil {
		t.Fatalf("registerMCP (2nd): %v", err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("second run was not a no-op:\nfirst=%s\nsecond=%s", first, second)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("unexpected .bak written on idempotent second run")
	}
}

// --- registerMCPJSON: VS Code's "servers" + "type":"stdio" shape ---

func TestRegisterMCPJSON_VSCodeFreshWritesTypeAndAbsolutePath(t *testing.T) {
	withFakeBinaryPath(t)
	cwd := t.TempDir()

	if err := registerMCP("vscode", cwd); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	var cfg map[string]any
	readJSON(t, filepath.Join(cwd, ".vscode", "mcp.json"), &cfg)
	entry := cfg["servers"].(map[string]any)["anchored"].(map[string]any)
	if entry["type"] != "stdio" {
		t.Errorf("type = %v, want stdio", entry["type"])
	}
	if entry["command"] != fakeBinaryPath {
		t.Errorf("command = %v, want %v", entry["command"], fakeBinaryPath)
	}
}

func TestRegisterMCPJSON_VSCodeRepairsBareCommand(t *testing.T) {
	withFakeBinaryPath(t)
	cwd := t.TempDir()
	path := filepath.Join(cwd, ".vscode", "mcp.json")
	writeJSON(t, path, map[string]any{
		"servers": map[string]any{
			"anchored": map[string]any{
				"type":    "stdio",
				"command": "anchored",
			},
		},
	})

	if err := registerMCP("vscode", cwd); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	var cfg map[string]any
	readJSON(t, path, &cfg)
	entry := cfg["servers"].(map[string]any)["anchored"].(map[string]any)
	if entry["command"] != fakeBinaryPath {
		t.Errorf("command = %v, want %v", entry["command"], fakeBinaryPath)
	}
	if entry["type"] != "stdio" {
		t.Errorf("type field lost during repair: %v", entry)
	}
}

// --- registerMCPTOML: Codex ---

func TestRegisterMCPTOML_FreshWritesAbsolutePath(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := registerMCP("codex", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	got := readFile(t, filepath.Join(home, ".codex", "config.toml"))
	want := `command = "` + fakeBinaryPath + `"`
	if !strings.Contains(got, want) {
		t.Errorf("config.toml = %q, want it to contain %q", got, want)
	}
}

func TestRegisterMCPTOML_RepairsBareCommandPreservesForeignSections(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "config.toml")

	initial := `[some_other_tool]
setting = "value"

[mcp_servers.anchored]
command = "anchored"
enabled = true

[another_tool]
foo = "bar"
`
	writeFile(t, path, initial)

	if err := registerMCP("codex", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	got := readFile(t, path)
	wantCmd := `command = "` + fakeBinaryPath + `"`
	if !strings.Contains(got, wantCmd) {
		t.Errorf("expected repaired command line %q in:\n%s", wantCmd, got)
	}
	if !strings.Contains(got, "[some_other_tool]") || !strings.Contains(got, `setting = "value"`) {
		t.Errorf("foreign section before anchored's table was lost:\n%s", got)
	}
	if !strings.Contains(got, "[another_tool]") || !strings.Contains(got, `foo = "bar"`) {
		t.Errorf("foreign section after anchored's table was lost:\n%s", got)
	}
	if !strings.Contains(got, "enabled = true") {
		t.Errorf("enabled field lost during repair:\n%s", got)
	}

	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("expected .bak backup, stat err=%v", err)
	}
}

func TestRegisterMCPTOML_SkipsAlreadyAbsoluteCommand(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "config.toml")

	initial := `[mcp_servers.anchored]
command = "/custom/path/anchored"
enabled = true
`
	writeFile(t, path, initial)
	before := readFile(t, path)

	if err := registerMCP("codex", "."); err != nil {
		t.Fatalf("registerMCP: %v", err)
	}

	after := readFile(t, path)
	if before != after {
		t.Errorf("file changed for an already-absolute custom command:\nbefore=%s\nafter=%s", before, after)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Errorf("unexpected .bak written when skipping an already-absolute command")
	}
}

func TestRegisterMCPTOML_IdempotentSecondRun(t *testing.T) {
	withFakeBinaryPath(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".codex", "config.toml")

	if err := registerMCP("codex", "."); err != nil {
		t.Fatalf("registerMCP (1st): %v", err)
	}
	first := readFile(t, path)

	if err := registerMCP("codex", "."); err != nil {
		t.Fatalf("registerMCP (2nd): %v", err)
	}
	second := readFile(t, path)

	if first != second {
		t.Errorf("second run was not a no-op:\nfirst=%s\nsecond=%s", first, second)
	}
}
