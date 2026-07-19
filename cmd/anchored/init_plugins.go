package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Deep host plugins. Unlike agentmemory (which talks to a REST server on
// :3111), these plugins shell out to the local `anchored` binary — recall via
// `anchored search`, capture via `anchored save` — so no daemon is needed.
// Each plugin body mirrors the files under configs/<host>/ and substitutes
// "<BIN>" with anchoredBinaryPath() at install time. go:embed can't reach the
// repo-root configs/ tree from this package, so the content is duplicated here
// and kept in sync by hand (same convention as init_cursor.go).

const openclawPluginManifest = `{
  "id": "anchored",
  "kind": "memory",
  "name": "anchored",
  "description": "Persistent cross-tool memory for OpenClaw via the local anchored binary.",
  "version": "0.14.0"
}
`

const openclawPluginPackageJSON = `{
  "name": "anchored-openclaw",
  "version": "0.14.0",
  "type": "module",
  "main": "plugin.mjs"
}
`

const openclawPluginMJS = `// anchored memory plugin for OpenClaw.
// Bridges the OpenClaw memory slot to the local ` + "`anchored`" + ` binary:
// recall before the agent starts, capture after it finishes. No REST server.
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const run = promisify(execFile);
const BIN = "<BIN>";

async function recall(prompt, cwd) {
  try {
    const { stdout } = await run(BIN, ["search", prompt || "", "--cwd", cwd || process.cwd()], { timeout: 5000 });
    return stdout && stdout.trim() ? "<anchored_memory>\n" + stdout.trim() + "\n</anchored_memory>" : "";
  } catch { return ""; }
}

async function capture(text) {
  if (!text) return;
  try { await run(BIN, ["save", text, "--category", "summary"], { timeout: 5000 }); } catch {}
}

export default function register(api) {
  // Claim the memory slot so OpenClaw treats anchored as the active provider.
  api.registerMemoryCapability?.({
    promptBuilder: async ({ prompt, cwd }) => recall(prompt, cwd),
  });
  api.on?.("agent_end", async (ctx) => capture(ctx?.summary || ctx?.lastMessage));
}
`

const hermesPluginYAML = `name: anchored
kind: memory
description: Persistent cross-tool memory for Hermes via the local anchored binary.
version: 0.14.0
entrypoint: __init__.py
`

const hermesPluginPy = `"""anchored memory provider for Hermes — bridges to the local ` + "`anchored`" + ` binary.

recall (prefetch / system_prompt_block) runs ` + "`anchored search`" + `; capture
(sync_turn / on_session_end) runs ` + "`anchored save`" + `. No REST server needed.
"""
import subprocess

BIN = "<BIN>"


def _run(args, timeout=5):
    try:
        return subprocess.run([BIN, *args], capture_output=True, text=True, timeout=timeout).stdout
    except Exception:
        return ""


def prefetch(prompt: str = "", cwd: str = "") -> str:
    out = _run(["search", prompt or "", "--cwd", cwd or "."]).strip()
    return f"<anchored_memory>\n{out}\n</anchored_memory>" if out else ""


def sync_turn(text: str = "") -> None:
    if text:
        _run(["save", text, "--category", "summary"])


def on_session_end(summary: str = "") -> None:
    if summary:
        _run(["save", summary, "--category", "summary"])
`

const piPluginTS = `// anchored extension for pi — bridges the agent lifecycle to the local
// ` + "`anchored`" + ` binary (recall before start, capture on end). Uses pi's
// extension API, not MCP.
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const run = promisify(execFile);
const BIN = "<BIN>";

async function recall(prompt: string, cwd: string): Promise<string> {
  try {
    const { stdout } = await run(BIN, ["search", prompt || "", "--cwd", cwd || process.cwd()], { timeout: 5000 });
    return stdout && stdout.trim() ? "<anchored_memory>\n" + stdout.trim() + "\n</anchored_memory>" : "";
  } catch { return ""; }
}

async function capture(text: string): Promise<void> {
  if (!text) return;
  try { await run(BIN, ["save", text, "--category", "summary"], { timeout: 5000 }); } catch {}
}

export default function register(api: any) {
  api.on?.("before_agent_start", async (ctx: any) => {
    const block = await recall(ctx?.prompt ?? "", ctx?.cwd ?? "");
    if (block && ctx?.injectContext) ctx.injectContext(block);
  });
  api.on?.("agent_end", async (ctx: any) => capture(ctx?.summary ?? ctx?.lastMessage ?? ""));
}
`

// writePluginFiles creates dir and writes each file, backing up any existing
// file to <path>.bak. Returns the first error encountered.
func writePluginFiles(dir string, files map[string]string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	bin := anchoredBinaryPath()
	for name, content := range files {
		path := filepath.Join(dir, name)
		body := strings.ReplaceAll(content, "<BIN>", bin)
		if prev, err := os.ReadFile(path); err == nil && string(prev) != body {
			_ = os.WriteFile(path+".bak", prev, 0644)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			return err
		}
	}
	return nil
}

// installOpenClawPlugin writes the plugin folder and claims the memory slot in
// ~/.openclaw/openclaw.json (plugins.slots.memory + plugins.entries.anchored).
func installOpenClawPlugin(home string) {
	dir := filepath.Join(home, ".openclaw", "extensions", "anchored")
	if err := writePluginFiles(dir, map[string]string{
		"openclaw.plugin.json": openclawPluginManifest,
		"package.json":         openclawPluginPackageJSON,
		"plugin.mjs":           openclawPluginMJS,
	}); err != nil {
		slog.Warn("failed to install openclaw plugin", "error", err)
		return
	}
	cfg := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := enableOpenClawSlot(cfg); err != nil {
		slog.Warn("failed to enable openclaw memory slot", "error", err)
		return
	}
	slog.Info("installed anchored OpenClaw plugin", "dir", dir)
}

// enableOpenClawSlot sets plugins.slots.memory="anchored" and
// plugins.entries.anchored.enabled=true, preserving all other config.
func enableOpenClawSlot(configPath string) error {
	doc := map[string]any{}
	prev, err := os.ReadFile(configPath)
	if err == nil && len(prev) > 0 {
		if e := json.Unmarshal(prev, &doc); e != nil {
			return e
		}
	}
	plugins, _ := doc["plugins"].(map[string]any)
	if plugins == nil {
		plugins = map[string]any{}
	}
	slots, _ := plugins["slots"].(map[string]any)
	if slots == nil {
		slots = map[string]any{}
	}
	slots["memory"] = "anchored"
	entries, _ := plugins["entries"].(map[string]any)
	if entries == nil {
		entries = map[string]any{}
	}
	entries["anchored"] = map[string]any{"enabled": true}
	plugins["slots"] = slots
	plugins["entries"] = entries
	doc["plugins"] = plugins

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return err
	}
	if len(prev) > 0 {
		_ = os.WriteFile(configPath+".bak", prev, 0644)
	}
	return os.WriteFile(configPath, append(out, '\n'), 0644)
}

// installHermesPlugin writes the Python plugin and sets memory.provider in
// ~/.hermes/config.yaml.
func installHermesPlugin(home string) {
	dir := filepath.Join(home, ".hermes", "plugins", "anchored")
	if err := writePluginFiles(dir, map[string]string{
		"plugin.yaml": hermesPluginYAML,
		"__init__.py": hermesPluginPy,
	}); err != nil {
		slog.Warn("failed to install hermes plugin", "error", err)
		return
	}
	cfg := filepath.Join(home, ".hermes", "config.yaml")
	doc, prev, err := readYAMLDoc(cfg)
	if err != nil {
		slog.Warn("failed to read hermes config", "error", err)
		return
	}
	mem, _ := doc["memory"].(map[string]any)
	if mem == nil {
		mem = map[string]any{}
	}
	mem["provider"] = "anchored"
	doc["memory"] = mem
	if err := writeYAMLDoc(cfg, doc, prev); err != nil {
		slog.Warn("failed to set hermes memory provider", "error", err)
		return
	}
	slog.Info("installed anchored Hermes plugin", "dir", dir)
}

// installPiPlugin writes the pi extension and registers it in
// ~/.pi/agent/settings.json extensions[].
func installPiPlugin(home string) {
	dir := filepath.Join(home, ".pi", "agent", "extensions", "anchored")
	if err := writePluginFiles(dir, map[string]string{"index.ts": piPluginTS}); err != nil {
		slog.Warn("failed to install pi extension", "error", err)
		return
	}
	settings := filepath.Join(home, ".pi", "agent", "settings.json")
	if err := registerPiExtension(settings, "~/.pi/agent/extensions/anchored"); err != nil {
		slog.Warn("failed to register pi extension", "error", err)
		return
	}
	slog.Info("installed anchored pi extension", "dir", dir)
}

// registerPiExtension appends extPath to the extensions[] array in settings.json
// if not already present, preserving other settings.
func registerPiExtension(settingsPath, extPath string) error {
	doc := map[string]any{}
	prev, err := os.ReadFile(settingsPath)
	if err == nil && len(prev) > 0 {
		if e := json.Unmarshal(prev, &doc); e != nil {
			return e
		}
	}
	list, _ := doc["extensions"].([]any)
	for _, it := range list {
		if s, _ := it.(string); s == extPath {
			return nil // already registered
		}
	}
	doc["extensions"] = append(list, extPath)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0755); err != nil {
		return err
	}
	if len(prev) > 0 {
		_ = os.WriteFile(settingsPath+".bak", prev, 0644)
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0644)
}
