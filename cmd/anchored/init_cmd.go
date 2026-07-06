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
	tool := fs.String("tool", "all", "Target tool: claude-code, cursor, opencode, agy, gemini, windsurf, cline, vscode, codex, devin, all")
	cwd := fs.String("cwd", "", "current working directory (used for workspace-scoped tools)")
	if err := fs.Parse(args); err != nil {
		fs.Usage()
		os.Exit(1)
	}

	tools := parseToolFlag(*tool)

	setupAnchored()
	ensureONNXModel()

	cwdVal := *cwd
	if cwdVal == "" {
		cwdVal = "."
	}

	for _, t := range tools {
		if !isToolInstalled(t, cwdVal) {
			slog.Info("tool not installed, skipping", "tool", t)
			continue
		}
		if err := registerMCP(t, cwdVal); err != nil {
			slog.Error("failed to register MCP", "tool", t, "error", err)
		}
		// Cursor needs more than MCP registration to activate: hooks.json
		// (capture + guards) and the always-on rule (the agent-side trigger,
		// since Cursor's beforeSubmitPrompt can't inject context).
		if t == "cursor" {
			if home, err := os.UserHomeDir(); err == nil {
				installCursorArtifacts(home)
			}
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
	case "agy":
		return []string{"agy"}
	case "gemini":
		return []string{"gemini"}
	case "windsurf":
		return []string{"windsurf"}
	case "cline":
		return []string{"cline"}
	case "vscode":
		return []string{"vscode"}
	case "codex":
		return []string{"codex"}
	case "devin":
		return []string{"devin"}
	case "all":
		return []string{"claude-code", "cursor", "opencode", "agy", "gemini", "windsurf", "cline", "vscode", "codex", "devin"}
	default:
		fmt.Fprintf(os.Stderr, "Unknown tool: %s\n", tool)
		os.Exit(1)
		return nil
	}
}

func isToolInstalled(t string, cwd string) bool {
	home, _ := os.UserHomeDir()
	switch t {
	case "claude-code":
		_, err := os.Stat(filepath.Join(home, ".claude"))
		return err == nil
	case "cursor":
		_, err := os.Stat(filepath.Join(home, ".cursor"))
		return err == nil
	case "opencode":
		_, err1 := os.Stat(filepath.Join(home, ".config", "opencode"))
		_, err2 := os.Stat(filepath.Join(home, ".local", "share", "opencode"))
		return err1 == nil || err2 == nil
	case "agy":
		_, err1 := os.Stat(filepath.Join(home, ".gemini", "config"))
		_, err2 := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli"))
		return err1 == nil || err2 == nil
	case "gemini":
		_, err := os.Stat(filepath.Join(home, ".gemini"))
		return err == nil
	case "windsurf":
		_, err := os.Stat(filepath.Join(home, ".codeium", "windsurf"))
		return err == nil
	case "cline":
		_, err1 := os.Stat(filepath.Join(home, ".cline"))
		_, err2 := os.Stat(filepath.Join(cwd, ".cline"))
		return err1 == nil || err2 == nil
	case "vscode":
		_, err := os.Stat(filepath.Join(cwd, ".vscode"))
		return err == nil
	case "codex":
		_, err := os.Stat(filepath.Join(home, ".codex"))
		return err == nil
	case "devin":
		_, err1 := os.Stat(filepath.Join(home, ".devin"))
		_, err2 := os.Stat(filepath.Join(cwd, ".devin"))
		return err1 == nil || err2 == nil
	}
	return false
}

func getToolMCPPath(t string, cwd string) string {
	home, _ := os.UserHomeDir()
	switch t {
	case "claude-code":
		return filepath.Join(home, ".claude.json")
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json")
	case "opencode":
		p := filepath.Join(home, ".config", "opencode", "opencode.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		return filepath.Join(home, ".local", "share", "opencode", "opencode.json")
	case "agy":
		p := filepath.Join(home, ".gemini", "config", "mcp_config.json")
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return p
		}
		return filepath.Join(home, ".gemini", "antigravity-cli", "mcp_config.json")
	case "gemini":
		return filepath.Join(home, ".gemini", "settings.json")
	case "windsurf":
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
	case "cline":
		p := filepath.Join(home, ".cline", "mcp.json")
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return p
		}
		return filepath.Join(cwd, ".cline", "mcp.json")
	case "vscode":
		return filepath.Join(cwd, ".vscode", "mcp.json")
	case "codex":
		return filepath.Join(home, ".codex", "config.toml")
	case "devin":
		p := filepath.Join(cwd, ".devin", "config.json")
		if _, err := os.Stat(filepath.Dir(p)); err == nil {
			return p
		}
		return filepath.Join(home, ".devin", "config.json")
	}
	return ""
}

// mcpConfig describes how a tool stores MCP server entries.
type mcpConfig struct {
	rootKey     string // "mcpServers" or "servers" (VS Code)
	requireType bool   // VS Code requires "type": "stdio"
	isTOML      bool   // Codex uses TOML
}

func getToolMCPConfig(t string) mcpConfig {
	switch t {
	case "vscode":
		return mcpConfig{rootKey: "servers", requireType: true}
	case "codex":
		return mcpConfig{isTOML: true}
	default:
		return mcpConfig{rootKey: "mcpServers"}
	}
}

func registerMCP(t string, cwd string) error {
	mc := getToolMCPConfig(t)

	if mc.isTOML {
		return registerMCPTOML(t, cwd)
	}
	return registerMCPJSON(t, cwd, mc)
}

func registerMCPJSON(t string, cwd string, mc mcpConfig) error {
	configPath := getToolMCPPath(t, cwd)
	if configPath == "" {
		return fmt.Errorf("no config path for %s", t)
	}

	var cfg map[string]json.RawMessage

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = make(map[string]json.RawMessage)
		} else {
			return fmt.Errorf("read %s: %w", configPath, err)
		}
	} else if len(strings.TrimSpace(string(data))) == 0 {
		cfg = make(map[string]json.RawMessage)
	} else {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", configPath, err)
		}
	}

	rootKey := mc.rootKey

	if serversRaw, ok := cfg[rootKey]; ok {
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(serversRaw, &servers); err == nil {
			if existingRaw, exists := servers["anchored"]; exists {
				// Repair mode: older anchored versions wrote a bare
				// "anchored" command, which only resolves via PATH and
				// breaks when a tool launches its MCP server outside a
				// login shell. Upgrade just the command field in place;
				// every other field (env, args, type, ...) round-trips
				// through json.RawMessage untouched.
				if repaired, changed := repairBareCommandEntry(existingRaw); changed {
					servers["anchored"] = repaired
					serversJSON, _ := json.Marshal(servers)
					cfg[rootKey] = serversJSON
					if err := writeMCPConfigFile(configPath, cfg, data); err != nil {
						return err
					}
					slog.Info("repaired anchored MCP command to absolute path", "tool", t, "path", configPath)
					return nil
				}
				slog.Info("already registered, skipping", "tool", t)
				return nil
			}
		}
	}

	var anchoredEntry json.RawMessage
	if mc.requireType {
		anchoredEntry, _ = json.Marshal(map[string]string{
			"type":    "stdio",
			"command": anchoredBinaryPath(),
		})
	} else {
		anchoredEntry, _ = json.Marshal(map[string]string{
			"command": anchoredBinaryPath(),
		})
	}

	var servers map[string]json.RawMessage
	if serversRaw, ok := cfg[rootKey]; ok {
		_ = json.Unmarshal(serversRaw, &servers)
	} else {
		servers = make(map[string]json.RawMessage)
	}
	servers["anchored"] = anchoredEntry

	serversJSON, _ := json.Marshal(servers)
	cfg[rootKey] = serversJSON

	if err := writeMCPConfigFile(configPath, cfg, data); err != nil {
		return err
	}

	slog.Info("registered anchored in MCP config", "tool", t, "path", configPath)
	return nil
}

// writeMCPConfigFile marshals cfg and writes it to configPath, backing up
// prevData to configPath+".bak" first when a file already existed there.
func writeMCPConfigFile(configPath string, cfg map[string]json.RawMessage, prevData []byte) error {
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if _, err := os.Stat(configPath); err == nil {
		_ = os.WriteFile(configPath+".bak", prevData, 0644)
	}

	if err := os.WriteFile(configPath, append(out, '\n'), 0644); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// repairBareCommandEntry rewrites the "command" field of a JSON MCP server
// entry to the resolved absolute binary path, but only when the existing
// command is exactly the bare "anchored" this tool used to write (PATH
// dependent). Every other field in the entry is preserved untouched via the
// json.RawMessage round-trip. Returns changed=false when the entry doesn't
// need repair (a custom or already-absolute command) or can't be parsed as
// an object, in which case the caller must leave it alone.
func repairBareCommandEntry(entryRaw json.RawMessage) (json.RawMessage, bool) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(entryRaw, &entry); err != nil {
		return nil, false
	}
	cmdRaw, ok := entry["command"]
	if !ok {
		return nil, false
	}
	var cmd string
	if err := json.Unmarshal(cmdRaw, &cmd); err != nil {
		return nil, false
	}
	if cmd != "anchored" {
		return nil, false
	}
	newCmd, err := json.Marshal(anchoredBinaryPath())
	if err != nil {
		return nil, false
	}
	entry["command"] = newCmd
	repaired, err := json.Marshal(entry)
	if err != nil {
		return nil, false
	}
	return repaired, true
}

func registerMCPTOML(t string, cwd string) error {
	configPath := getToolMCPPath(t, cwd)
	if configPath == "" {
		return fmt.Errorf("no config path for %s", t)
	}

	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}

	if data != nil {
		lines := strings.Split(string(data), "\n")
		sectionIdx := -1
		for i, line := range lines {
			if strings.TrimSpace(line) == "[mcp_servers.anchored]" {
				sectionIdx = i
				break
			}
		}
		if sectionIdx != -1 {
			// Repair mode: mirror of the JSON path above. Only the
			// `command = "anchored"` line inside this table is rewritten;
			// every other line (including foreign tables elsewhere in the
			// file) is byte-preserved.
			if repaired, changed := repairTOMLBareCommand(lines, sectionIdx); changed {
				_ = os.WriteFile(configPath+".bak", data, 0644)
				out := strings.Join(repaired, "\n")
				if err := os.WriteFile(configPath, []byte(out), 0644); err != nil {
					return fmt.Errorf("write %s: %w", configPath, err)
				}
				slog.Info("repaired anchored MCP command to absolute path", "tool", t, "path", configPath)
				return nil
			}
			slog.Info("already registered, skipping", "tool", t)
			return nil
		}
	}

	entry := fmt.Sprintf("[mcp_servers.anchored]\ncommand = %q\nenabled = true\n", anchoredBinaryPath())

	if _, err := os.Stat(configPath); err == nil {
		_ = os.WriteFile(configPath+".bak", data, 0644)
	}

	f, err := os.OpenFile(configPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", configPath, err)
	}
	defer f.Close()

	if len(data) > 0 && !strings.HasSuffix(string(data), "\n\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("write %s: %w", configPath, err)
		}
	}

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}

	slog.Info("registered anchored in MCP config", "tool", t, "path", configPath)
	return nil
}

// repairTOMLBareCommand scans the [mcp_servers.anchored] table starting at
// sectionIdx for a `command = "anchored"` line (bare, PATH-dependent) and
// rewrites it in place to the resolved absolute binary path. The table ends
// at the next line starting with "[" or at EOF. Returns changed=false when
// no bare command line is found (already absolute or a custom command),
// leaving lines untouched.
func repairTOMLBareCommand(lines []string, sectionIdx int) ([]string, bool) {
	for i := sectionIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "[") {
			break // start of the next table
		}
		if trimmed == `command = "anchored"` {
			lines[i] = fmt.Sprintf("command = %q", anchoredBinaryPath())
			return lines, true
		}
	}
	return lines, false
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
