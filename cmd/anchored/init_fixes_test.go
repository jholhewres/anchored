package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The full init sequence (registerMCP then installPlugin) writes openclaw.json
// twice; the .bak must still hold the pre-init original, not the intermediate.
func TestInit_BakHoldsOriginalAcrossDoubleWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0755); err != nil {
		t.Fatal(err)
	}
	original := `{"model":"gpt"}`
	if err := os.WriteFile(cfg, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	if err := registerMCP("openclaw", "."); err != nil { // write #1 (+ .bak = original)
		t.Fatalf("registerMCP: %v", err)
	}
	installOpenClawPlugin(home) // write #2 (must NOT clobber .bak)

	bak, err := os.ReadFile(cfg + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if strings.TrimSpace(string(bak)) != original {
		t.Fatalf(".bak should hold the original %q, got %q", original, string(bak))
	}
}

// Foreign integer fields larger than 2^53 must survive the openclaw.json edit
// (json.Number, not float64).
func TestEnableOpenClawSlot_PreservesLargeInt(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "openclaw.json")
	if err := os.WriteFile(cfg, []byte(`{"snowflake":123456789012345678,"plugins":{}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := enableOpenClawSlot(cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	if !strings.Contains(string(data), "123456789012345678") {
		t.Fatalf("large int corrupted: %s", data)
	}
}

// Re-running must not wipe sibling fields under plugins.entries.anchored.
func TestEnableOpenClawSlot_MergesEntry(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "openclaw.json")
	seed := `{"plugins":{"entries":{"anchored":{"apiKey":"keep-me"}}}}`
	if err := os.WriteFile(cfg, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	if err := enableOpenClawSlot(cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	entry := doc["plugins"].(map[string]any)["entries"].(map[string]any)["anchored"].(map[string]any)
	if entry["apiKey"] != "keep-me" {
		t.Errorf("sibling field apiKey dropped: %v", entry)
	}
	if entry["enabled"] != true {
		t.Errorf("enabled not set: %v", entry)
	}
}

// The pi extension path in settings.json must be absolute, not tilde-prefixed.
func TestInstallPiPlugin_AbsolutePath(t *testing.T) {
	home := t.TempDir()
	installPiPlugin(home)
	data, _ := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	exts := doc["extensions"].([]any)
	got, _ := exts[0].(string)
	if strings.HasPrefix(got, "~") || !filepath.IsAbs(got) {
		t.Fatalf("pi extension path not absolute: %q", got)
	}
}

// A config that merely mentions "anchored" as an unrelated value must NOT be
// reported as registered.
func TestIsAnchoredRegistered_NoFalsePositive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oc := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(oc, 0755); err != nil {
		t.Fatal(err)
	}
	// "anchored" appears only as a label, not as a server entry.
	if err := os.WriteFile(filepath.Join(oc, "openclaw.json"), []byte(`{"label":"anchored","mcpServers":{"other":{"command":"x"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if isAnchoredRegistered("openclaw", ".") {
		t.Fatal("false positive: anchored is only a label, not a registered server")
	}
	// Now add a real entry → should be true.
	if err := os.WriteFile(filepath.Join(oc, "openclaw.json"), []byte(`{"mcpServers":{"anchored":{"command":"x"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if !isAnchoredRegistered("openclaw", ".") {
		t.Fatal("real anchored server entry not detected")
	}
}
