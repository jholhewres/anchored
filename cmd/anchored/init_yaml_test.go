package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// withTempHome points HOME at a temp dir for the duration of the test so
// getToolMCPPath resolves under it.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

func TestRegisterMCP_HermesYAMLMap(t *testing.T) {
	home := withTempHome(t)
	cfgPath := filepath.Join(home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config with a foreign key and a foreign MCP server.
	seed := "memory:\n  provider: hermes\nmcp_servers:\n  other:\n    command: othercmd\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	if err := registerMCP("hermes", "."); err != nil {
		t.Fatalf("registerMCP hermes: %v", err)
	}

	doc := readYAML(t, cfgPath)
	// Foreign top-level key preserved.
	mem, _ := doc["memory"].(map[string]any)
	if mem == nil || mem["provider"] != "hermes" {
		t.Fatalf("foreign key memory.provider not preserved: %v", doc["memory"])
	}
	servers, _ := doc["mcp_servers"].(map[string]any)
	if servers == nil || servers["other"] == nil {
		t.Fatalf("foreign server 'other' dropped: %v", servers)
	}
	anchored, _ := servers["anchored"].(map[string]any)
	if anchored == nil || anchored["command"] == "" {
		t.Fatalf("anchored not registered: %v", servers)
	}
	// .bak written.
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Errorf("expected .bak: %v", err)
	}

	// Idempotent: second run leaves exactly one anchored entry unchanged.
	if err := registerMCP("hermes", "."); err != nil {
		t.Fatalf("2nd registerMCP: %v", err)
	}
	doc2 := readYAML(t, cfgPath)
	s2, _ := doc2["mcp_servers"].(map[string]any)
	if len(s2) != 2 {
		t.Fatalf("expected 2 servers after idempotent run, got %d: %v", len(s2), s2)
	}
}

func TestRegisterMCP_ClawYAMLArray(t *testing.T) {
	home := withTempHome(t)
	cfgPath := filepath.Join(home, ".devclaw", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}
	seed := "llm:\n  provider: openai\nmcp:\n  enabled: true\n  servers:\n    - name: other\n      type: stdio\n      command: othercmd\n"
	if err := os.WriteFile(cfgPath, []byte(seed), 0644); err != nil {
		t.Fatal(err)
	}

	if err := registerMCP("devclaw", "."); err != nil {
		t.Fatalf("registerMCP devclaw: %v", err)
	}

	doc := readYAML(t, cfgPath)
	if llm, _ := doc["llm"].(map[string]any); llm == nil || llm["provider"] != "openai" {
		t.Fatalf("foreign key llm dropped: %v", doc["llm"])
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		t.Fatalf("mcp section missing")
	}
	list, _ := mcp["servers"].([]any)
	if len(list) != 2 {
		t.Fatalf("expected 2 servers (other + anchored), got %d: %v", len(list), list)
	}
	var foundAnchored, foundOther bool
	for _, it := range list {
		e, _ := it.(map[string]any)
		switch e["name"] {
		case "anchored":
			foundAnchored = true
			if e["type"] != "stdio" || e["command"] == "" || e["enabled"] != true {
				t.Fatalf("anchored entry malformed: %v", e)
			}
		case "other":
			foundOther = true
		}
	}
	if !foundAnchored || !foundOther {
		t.Fatalf("missing entries: anchored=%v other=%v", foundAnchored, foundOther)
	}

	// Idempotent.
	if err := registerMCP("devclaw", "."); err != nil {
		t.Fatalf("2nd registerMCP: %v", err)
	}
	doc2 := readYAML(t, cfgPath)
	mcp2, _ := doc2["mcp"].(map[string]any)
	list2, _ := mcp2["servers"].([]any)
	if len(list2) != 2 {
		t.Fatalf("expected 2 servers after idempotent run, got %d", len(list2))
	}
}

func TestRegisterMCP_OpenClawJSONMap(t *testing.T) {
	home := withTempHome(t)
	cfgPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
		t.Fatal(err)
	}

	if err := registerMCP("openclaw", "."); err != nil {
		t.Fatalf("registerMCP openclaw: %v", err)
	}
	doc := readYAML(t, cfgPath) // JSON is valid YAML, so this parses fine
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil || servers["anchored"] == nil {
		t.Fatalf("anchored not registered under mcpServers: %v", doc)
	}
}
