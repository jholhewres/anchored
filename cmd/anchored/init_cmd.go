package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

func runInit(args []string) {
	fs := newFlagSet("init")
	tool := fs.String("tool", "all", "Target tool: claude-code, cursor, opencode, all")
	if err := fs.Parse(args); err != nil {
		fs.Usage()
		os.Exit(1)
	}

	tools := parseToolFlag(*tool)

	setupAnchored()
	ensureONNXModel()

	for _, t := range tools {
		if !isToolInstalled(t) {
			slog.Info("tool not installed, skipping", "tool", t)
			continue
		}
		if err := registerMCP(t); err != nil {
			slog.Error("failed to register MCP", "tool", t, "error", err)
		}
	}

	fmt.Fprintln(os.Stderr, "\nAnchored initialized. Restart your tool to pick up the MCP server.")
}

func parseToolFlag(tool string) []string {
	switch strings.ToLower(tool) {
	case "claude-code":
		return []string{"claude-code"}
	case "cursor":
		return []string{"cursor"}
	case "opencode":
		return []string{"opencode"}
	case "all":
		return []string{"claude-code", "cursor", "opencode"}
	default:
		fmt.Fprintf(os.Stderr, "Unknown tool: %s\n", tool)
		os.Exit(1)
		return nil
	}
}

type toolInfo struct {
	name       string
	configPath string
}

func isToolInstalled(t string) bool {
	home, _ := os.UserHomeDir()
	switch t {
	case "claude-code":
		// Check for .claude directory or any config file
		p := filepath.Join(home, ".claude")
		_, err := os.Stat(p)
		return err == nil
	case "cursor":
		p := filepath.Join(home, ".cursor")
		_, err := os.Stat(p)
		return err == nil
	case "opencode":
		p1 := filepath.Join(home, ".config", "opencode")
		p2 := filepath.Join(home, ".local", "share", "opencode")
		_, err1 := os.Stat(p1)
		_, err2 := os.Stat(p2)
		return err1 == nil || err2 == nil
	}
	return false
}

func getToolMCPPath(t string) string {
	home, _ := os.UserHomeDir()
	switch t {
	case "claude-code":
		return filepath.Join(home, ".claude", "mcp.json")
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json")
	case "opencode":
		p := filepath.Join(home, ".config", "opencode", "opencode.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return filepath.Join(home, ".local", "share", "opencode", "opencode.json")
	}
	return ""
}

func registerMCP(t string) error {
	configPath := getToolMCPPath(t)
	if configPath == "" {
		return fmt.Errorf("no config path for %s", t)
	}

	// Read existing config
	var cfg map[string]json.RawMessage

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = make(map[string]json.RawMessage)
		} else {
			return fmt.Errorf("read %s: %w", configPath, err)
		}
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", configPath, err)
		}
	}

	// Check if already registered
	if serversRaw, ok := cfg["mcpServers"]; ok {
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(serversRaw, &servers); err == nil {
			if _, exists := servers["anchored"]; exists {
				slog.Info("already registered, skipping", "tool", t)
				return nil
			}
		}
	}

	// Build anchored entry
	anchoredEntry, _ := json.Marshal(map[string]string{
		"command": "anchored",
	})

	// Merge mcpServers
	var servers map[string]json.RawMessage
	if serversRaw, ok := cfg["mcpServers"]; ok {
		_ = json.Unmarshal(serversRaw, &servers)
	} else {
		servers = make(map[string]json.RawMessage)
	}
	servers["anchored"] = anchoredEntry

	serversJSON, _ := json.Marshal(servers)
	cfg["mcpServers"] = serversJSON

	// Write back
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, append(out, '\n'), 0644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	slog.Info("registered anchored in MCP config", "tool", t, "path", configPath)
	return nil
}

func setupAnchored() {
	home, _ := os.UserHomeDir()
	anchoredDir := filepath.Join(home, ".anchored")

	if err := os.MkdirAll(anchoredDir, 0755); err != nil {
		slog.Warn("failed to create ~/.anchored", "error", err)
		return
	}

	identityPath := filepath.Join(anchoredDir, "identity.md")
	if _, err := os.Stat(identityPath); err == nil {
		return // already exists
	}

	const identityTemplate = `# Identity

## About Me
- Name: 
- Role: 
- Preferences: 

## Projects
- 
`

	if err := os.WriteFile(identityPath, []byte(identityTemplate), 0644); err != nil {
		slog.Warn("failed to create identity.md", "error", err)
		return
	}

	slog.Info("created ~/.anchored/identity.md")
}

func ensureONNXModel() {
	home, _ := os.UserHomeDir()
	onnxDir := filepath.Join(home, ".anchored", "data", "onnx")

	// Check if model file exists
	entries, err := os.ReadDir(onnxDir)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("ONNX model not found. Run 'anchored' to auto-download the embedding model.")
		} else {
			slog.Warn("failed to check ONNX model directory", "error", err)
		}
		return
	}

	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".onnx") {
			return // model found
		}
	}

	slog.Info("ONNX model not found. Run 'anchored' to auto-download the embedding model.")
}
