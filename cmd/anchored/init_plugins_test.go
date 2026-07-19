package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInstallOpenClawPlugin(t *testing.T) {
	home := t.TempDir()
	installOpenClawPlugin(home)

	dir := filepath.Join(home, ".openclaw", "extensions", "anchored")
	for _, f := range []string{"openclaw.plugin.json", "package.json", "plugin.mjs"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing plugin file %s: %v", f, err)
		}
	}
	// <BIN> substituted with the resolved binary path.
	mjs, _ := os.ReadFile(filepath.Join(dir, "plugin.mjs"))
	if strings.Contains(string(mjs), "<BIN>") {
		t.Error("plugin.mjs still contains <BIN> placeholder")
	}
	if !strings.Contains(string(mjs), anchoredBinaryPath()) {
		t.Error("plugin.mjs missing resolved binary path")
	}

	// Memory slot claimed in openclaw.json.
	data, err := os.ReadFile(filepath.Join(home, ".openclaw", "openclaw.json"))
	if err != nil {
		t.Fatalf("read openclaw.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse openclaw.json: %v", err)
	}
	plugins, _ := doc["plugins"].(map[string]any)
	slots, _ := plugins["slots"].(map[string]any)
	if slots["memory"] != "anchored" {
		t.Errorf("plugins.slots.memory = %v, want anchored", slots["memory"])
	}
	entries, _ := plugins["entries"].(map[string]any)
	if entries["anchored"] == nil {
		t.Error("plugins.entries.anchored missing")
	}
}

func TestInstallOpenClawPlugin_PreservesForeignConfig(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config with a foreign top-level key + a foreign slot.
	seed := `{"model":"gpt","plugins":{"slots":{"tools":"builtin"}}}`
	if err := os.WriteFile(cfg, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}
	installOpenClawPlugin(home)

	data, _ := os.ReadFile(cfg)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	if doc["model"] != "gpt" {
		t.Errorf("foreign key model dropped: %v", doc["model"])
	}
	plugins, _ := doc["plugins"].(map[string]any)
	slots, _ := plugins["slots"].(map[string]any)
	if slots["tools"] != "builtin" {
		t.Errorf("foreign slot tools dropped: %v", slots)
	}
	if slots["memory"] != "anchored" {
		t.Errorf("memory slot not set: %v", slots)
	}
	if _, err := os.Stat(cfg + ".bak"); err != nil {
		t.Errorf("expected .bak: %v", err)
	}
}

func TestInstallHermesPlugin(t *testing.T) {
	home := t.TempDir()
	installHermesPlugin(home)

	dir := filepath.Join(home, ".hermes", "plugins", "anchored")
	for _, f := range []string{"plugin.yaml", "__init__.py"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	py, _ := os.ReadFile(filepath.Join(dir, "__init__.py"))
	if strings.Contains(string(py), "<BIN>") {
		t.Error("__init__.py still has <BIN>")
	}
	data, err := os.ReadFile(filepath.Join(home, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatalf("read hermes config: %v", err)
	}
	var doc map[string]any
	_ = yaml.Unmarshal(data, &doc)
	mem, _ := doc["memory"].(map[string]any)
	if mem["provider"] != "anchored" {
		t.Errorf("memory.provider = %v, want anchored", mem["provider"])
	}
}

func TestInstallPiPlugin(t *testing.T) {
	home := t.TempDir()
	installPiPlugin(home)

	idx := filepath.Join(home, ".pi", "agent", "extensions", "anchored", "index.ts")
	if _, err := os.Stat(idx); err != nil {
		t.Fatalf("missing index.ts: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	exts, _ := doc["extensions"].([]any)
	found := false
	for _, e := range exts {
		if s, _ := e.(string); strings.Contains(s, "anchored") {
			found = true
		}
	}
	if !found {
		t.Errorf("anchored not registered in extensions: %v", exts)
	}

	// Idempotent: second install doesn't duplicate the extension entry.
	installPiPlugin(home)
	data2, _ := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
	var doc2 map[string]any
	_ = json.Unmarshal(data2, &doc2)
	exts2, _ := doc2["extensions"].([]any)
	if len(exts2) != 1 {
		t.Errorf("expected 1 extension after idempotent install, got %d", len(exts2))
	}
}
