package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readCursorHooks(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("parse hooks.json: %v", err)
	}
	return top
}

func cursorEventCommands(t *testing.T, top map[string]any, event string) []string {
	t.Helper()
	hooks, _ := top["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	var cmds []string
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok {
			if c, ok := m["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func TestInstallCursorArtifacts_FreshInstall(t *testing.T) {
	home := t.TempDir()
	installCursorArtifacts(home)

	raw, err := os.ReadFile(filepath.Join(home, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("hooks.json not created: %v", err)
	}
	if strings.Contains(string(raw), "<BIN>") {
		t.Fatal("hooks.json still contains the <BIN> placeholder")
	}

	top := readCursorHooks(t, home)
	bin := anchoredBinaryPath()
	for _, ev := range cursorHookEvents {
		cmds := cursorEventCommands(t, top, ev.event)
		if len(cmds) != 1 {
			t.Fatalf("event %s: expected 1 entry, got %d", ev.event, len(cmds))
		}
		want := bin + " hook " + ev.subcommand
		if cmds[0] != want {
			t.Errorf("event %s: command = %q, want %q", ev.event, cmds[0], want)
		}
	}

	mdc, err := os.ReadFile(filepath.Join(home, ".cursor", "rules", "anchored.mdc"))
	if err != nil {
		t.Fatalf("anchored.mdc not created: %v", err)
	}
	if string(mdc) != cursorRuleTemplate {
		t.Error("anchored.mdc content differs from the embedded template")
	}
}

func TestInstallCursorHooks_MergePreservesForeignEntry(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"version":1,"hooks":{"stop":[{"command":"gitbutler stop"}]}}`
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installCursorHooks(home); err != nil {
		t.Fatalf("installCursorHooks: %v", err)
	}

	top := readCursorHooks(t, home)
	stopCmds := cursorEventCommands(t, top, "stop")
	if len(stopCmds) != 2 {
		t.Fatalf("stop: expected foreign + anchored entries, got %v", stopCmds)
	}
	if stopCmds[0] != "gitbutler stop" {
		t.Errorf("foreign entry not preserved in position: %v", stopCmds)
	}
	if !strings.HasSuffix(stopCmds[1], " hook stop") {
		t.Errorf("anchored stop entry missing: %v", stopCmds)
	}

	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Error(".bak not written before modifying existing hooks.json")
	}
	bak, _ := os.ReadFile(path + ".bak")
	if string(bak) != pre {
		t.Error(".bak does not contain the pre-merge content")
	}
}

func TestInstallCursorHooks_RepairsBareCommand(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"version":1,"hooks":{"stop":[{"command":"anchored hook stop"}]}}`
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installCursorHooks(home); err != nil {
		t.Fatalf("installCursorHooks: %v", err)
	}

	top := readCursorHooks(t, home)
	stopCmds := cursorEventCommands(t, top, "stop")
	if len(stopCmds) != 1 {
		t.Fatalf("stop: bare entry must be repaired, not duplicated: %v", stopCmds)
	}
	bin := anchoredBinaryPath()
	if stopCmds[0] != bin+" hook stop" {
		t.Errorf("bare command not repaired to absolute path: %q", stopCmds[0])
	}
}

func TestInstallCursorHooks_DedupIgnoresPathPrefixAndFlags(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".cursor", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	pre := `{"version":1,"hooks":{"stop":[{"command":"/some/other/path/anchored hook stop --config /x/y.yaml"}]}}`
	if err := os.WriteFile(path, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := installCursorHooks(home); err != nil {
		t.Fatalf("installCursorHooks: %v", err)
	}

	top := readCursorHooks(t, home)
	stopCmds := cursorEventCommands(t, top, "stop")
	if len(stopCmds) != 1 {
		t.Fatalf("stop: pathed+flagged anchored entry must dedup, got %v", stopCmds)
	}
	if stopCmds[0] != "/some/other/path/anchored hook stop --config /x/y.yaml" {
		t.Errorf("existing absolute entry must be left untouched: %q", stopCmds[0])
	}
}

func TestInstallCursorArtifacts_IdempotentSecondRun(t *testing.T) {
	home := t.TempDir()
	installCursorArtifacts(home)

	hooksPath := filepath.Join(home, ".cursor", "hooks.json")
	mdcPath := filepath.Join(home, ".cursor", "rules", "anchored.mdc")
	hooks1, _ := os.ReadFile(hooksPath)
	mdc1, _ := os.ReadFile(mdcPath)

	installCursorArtifacts(home)

	hooks2, _ := os.ReadFile(hooksPath)
	mdc2, _ := os.ReadFile(mdcPath)
	if string(hooks1) != string(hooks2) {
		t.Error("second run changed hooks.json (not idempotent)")
	}
	if string(mdc1) != string(mdc2) {
		t.Error("second run changed anchored.mdc (not idempotent)")
	}
	if _, err := os.Stat(hooksPath + ".bak"); err == nil {
		t.Error("idempotent second run must not write a .bak")
	}
}

// The embedded templates must stay in sync with the files shipped under
// configs/cursor/ — init installs the embedded copy, doctor checks what the
// configs describe, and drift between them would install one thing while
// documenting another. go:embed can't reach ../../configs from this package,
// hence the runtime comparison.
func TestCursorTemplates_MatchConfigsDir(t *testing.T) {
	repoHooks, err := os.ReadFile("../../configs/cursor/hooks.json")
	if err != nil {
		t.Skipf("configs dir not available: %v", err)
	}
	if string(repoHooks) != cursorHooksTemplate {
		t.Error("cursorHooksTemplate drifted from configs/cursor/hooks.json")
	}
	repoRule, err := os.ReadFile("../../configs/cursor/anchored.mdc")
	if err != nil {
		t.Skipf("configs dir not available: %v", err)
	}
	if string(repoRule) != cursorRuleTemplate {
		t.Error("cursorRuleTemplate drifted from configs/cursor/anchored.mdc")
	}
}
