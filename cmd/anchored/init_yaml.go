package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/jholhewres/anchored/pkg/config"
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

// writeYAMLDoc marshals doc to configPath, creating parent dirs and preserving
// the user's original in configPath+".bak".
//
// NOTE: this round-trips through a generic map, so it preserves the config's
// DATA but not YAML comments or key ordering — the rewritten file is
// alphabetized and stripped of comments. The .bak (the true original, kept via
// backupOnce) is the recovery path. A single `anchored init` run may write the
// same file twice (MCP registration then a plugin edit); backupOnce ensures the
// .bak still holds the pre-init original after both writes.
func writeYAMLDoc(configPath string, doc map[string]any, prev []byte) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal yaml: %w", err)
	}
	backupOnce(configPath, prev)
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// writePrivateFile writes a file that may hold credentials (the config and
// its .bak carry remote API keys) owner-only. os.WriteFile applies its mode
// only when it creates the file, so an existing file is tightened as well.
func writePrivateFile(path string, data []byte) error {
	// Tighten first: writing into an existing 0664 file and fixing the mode
	// afterwards would leave the new keys readable in between.
	if _, err := config.TightenPerm(path, config.PrivateFileMode); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, config.PrivateFileMode); err != nil {
		return err
	}
	_, err := config.TightenPerm(path, config.PrivateFileMode)
	return err
}

// backupOnce writes prev to path+".bak" only if the target file exists and no
// .bak is present yet. This keeps the .bak pinned to the user's ORIGINAL config
// even when `anchored init` rewrites the same file more than once in a run
// (e.g. MCP registration followed by a plugin edit) — a later write must not
// clobber the backup with intermediate content.
func backupOnce(path string, prev []byte) {
	if _, err := os.Stat(path); err != nil {
		return // no original file → nothing to back up
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		// Backup already captured the original; still bring an older
		// version's world-readable copy down to the original's mode.
		tightenBackup(path)
		return
	}
	writeBackupFile(path, prev)
}

// writeBackupFile writes data to path+".bak" with the original's mode:
// another tool's config can hold its own secrets, and a 0644 copy would
// expose what a 0600 original did not. An existing backup is tightened
// before it is overwritten.
func writeBackupFile(path string, data []byte) {
	mode := config.PrivateFileMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	bak := path + ".bak"
	_, _ = config.TightenPerm(bak, mode)
	if err := os.WriteFile(bak, data, mode); err != nil {
		return
	}
	_, _ = config.TightenPerm(bak, mode)
}

func tightenBackup(path string) {
	if info, err := os.Stat(path); err == nil {
		_, _ = config.TightenPerm(path+".bak", info.Mode().Perm())
	}
}
