package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// registerMCPYAMLMap registers anchored in a host whose MCP servers live in a
// top-level YAML map keyed by server name (Hermes: `mcp_servers`). It round-trips
// the whole document through a generic map so foreign keys survive untouched,
// backs up the original, and is idempotent (an existing `anchored` entry with an
// absolute command is left alone; a bare `anchored` command is repaired).
func registerMCPYAMLMap(t, cwd, rootKey string) error {
	configPath := getToolMCPPath(t, cwd)
	if configPath == "" {
		return fmt.Errorf("no config path for %s", t)
	}

	doc, prev, err := readYAMLDoc(configPath)
	if err != nil {
		return err
	}

	servers, _ := doc[rootKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	if existing, ok := servers["anchored"].(map[string]any); ok {
		if cmd, _ := existing["command"].(string); cmd == "anchored" {
			existing["command"] = anchoredBinaryPath()
			servers["anchored"] = existing
			doc[rootKey] = servers
			if err := writeYAMLDoc(configPath, doc, prev); err != nil {
				return err
			}
			slog.Info("repaired anchored MCP command to absolute path", "tool", t, "path", configPath)
			return nil
		}
		slog.Info("already registered, skipping", "tool", t)
		return nil
	}

	servers["anchored"] = map[string]any{"command": anchoredBinaryPath()}
	doc[rootKey] = servers
	if err := writeYAMLDoc(configPath, doc, prev); err != nil {
		return err
	}
	slog.Info("registered anchored in MCP config", "tool", t, "path", configPath)
	return nil
}

// registerMCPYAMLArray registers anchored in a host whose MCP servers live in a
// YAML list of objects under a nested key (claw-family: `mcp.servers`, each an
// object with a `name` field). It upserts an entry named "anchored" without
// touching sibling servers or other config keys.
func registerMCPYAMLArray(t, cwd, rootKey string) error {
	configPath := getToolMCPPath(t, cwd)
	if configPath == "" {
		return fmt.Errorf("no config path for %s", t)
	}

	doc, prev, err := readYAMLDoc(configPath)
	if err != nil {
		return err
	}

	section, _ := doc[rootKey].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	rawList, _ := section["servers"].([]any)

	for i, item := range rawList {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := entry["name"].(string); name == "anchored" {
			if cmd, _ := entry["command"].(string); cmd == "anchored" {
				entry["command"] = anchoredBinaryPath()
				rawList[i] = entry
				section["servers"] = rawList
				doc[rootKey] = section
				if err := writeYAMLDoc(configPath, doc, prev); err != nil {
					return err
				}
				slog.Info("repaired anchored MCP command to absolute path", "tool", t, "path", configPath)
				return nil
			}
			slog.Info("already registered, skipping", "tool", t)
			return nil
		}
	}

	rawList = append(rawList, map[string]any{
		"name":    "anchored",
		"type":    "stdio",
		"command": anchoredBinaryPath(),
		"enabled": true,
	})
	section["servers"] = rawList
	doc[rootKey] = section
	if err := writeYAMLDoc(configPath, doc, prev); err != nil {
		return err
	}
	slog.Info("registered anchored in MCP config", "tool", t, "path", configPath)
	return nil
}

// readYAMLDoc reads a YAML config into a generic map. A missing or empty file
// yields an empty document. Returns the parsed doc plus the original bytes (for
// the .bak written on write).
func readYAMLDoc(configPath string) (map[string]any, []byte, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil, nil
		}
		return nil, nil, fmt.Errorf("read %s: %w", configPath, err)
	}
	doc := map[string]any{}
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", configPath, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	}
	return doc, data, nil
}

// writeYAMLDoc marshals doc to configPath, creating parent dirs and backing up
// prev (the prior file bytes) to configPath+".bak" when a file existed.
func writeYAMLDoc(configPath string, doc map[string]any, prev []byte) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	if _, statErr := os.Stat(configPath); statErr == nil {
		_ = os.WriteFile(configPath+".bak", prev, 0644)
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}
