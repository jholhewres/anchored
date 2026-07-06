package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pluginRegistryKey is the entry under installed_plugins.json["plugins"] that
// Claude Code uses to identify our plugin. Format is "<plugin>@<marketplace>"
// where both segments come from the marketplace JSON. Hard-coded because the
// plugin and marketplace are both named "anchored".
const pluginRegistryKey = "anchored@anchored"

// supportedRegistrySchema is the only version of installed_plugins.json we
// know how to mutate safely. When Claude Code bumps the schema we abort and
// fall back to the manual-fix notice, so anchored never corrupts state for
// other plugins by mis-writing fields.
const supportedRegistrySchema = 2

// installPluginFromMirror replicates Claude Code's `/plugin install` for the
// happy path: copies the marketplace working tree into the version-stamped
// cache directory and rewrites installed_plugins.json so Claude Code loads
// the new version on its next launch.
//
// All file writes go through tmp+rename so a crash mid-install cannot leave
// either the cache or the registry half-written.
//
// Aborts (returning error) when:
//   - installed_plugins.json schema is not the supported version
//   - registry is unreadable / unparseable
//   - copy/rename fails for any reason
//
// The caller (applyPluginAutoUpdate) renders a graceful fallback notice on
// error; the user keeps the working v0.4.9-style /plugin install command.
func installPluginFromMirror(mirrorDir, cacheParentDir, registryPath, version string) error {
	if mirrorDir == "" || cacheParentDir == "" || registryPath == "" || version == "" {
		return errors.New("missing required path")
	}

	// Pre-flight: read + validate the registry BEFORE touching the cache.
	// A schema mismatch must abort here so we never end up with a populated
	// cache that the registry can't reach.
	doc, err := loadPluginRegistry(registryPath)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	if doc.Version != supportedRegistrySchema {
		return fmt.Errorf("registry schema v%d not supported (need v%d)", doc.Version, supportedRegistrySchema)
	}

	// Copy the marketplace tree to a sibling tmp dir, then atomically rename.
	finalDir := filepath.Join(cacheParentDir, version)
	tmpDir := filepath.Join(cacheParentDir, "."+version+".tmp")
	if err := os.RemoveAll(tmpDir); err != nil {
		return fmt.Errorf("clean prior tmp: %w", err)
	}
	if err := os.MkdirAll(cacheParentDir, 0o755); err != nil {
		return fmt.Errorf("ensure cache parent: %w", err)
	}
	if err := copyDirExcludingGit(mirrorDir, tmpDir); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("copy mirror tree: %w", err)
	}

	// Rewrite hook commands in the installed copy to an absolute binary path
	// (see anchoredBinaryPath) so hooks still fire when Claude Code launches
	// them outside a login shell with no PATH. The repo template stays
	// generic — it ships to every user — only this per-machine cached copy
	// is patched. Non-fatal: hooks.json is optional and any failure here
	// must not abort the install; the plugin still resolves "anchored" via
	// PATH in that case.
	rewriteInstalledHookCommands(filepath.Join(tmpDir, "hooks", "hooks.json"))

	// If a directory already exists at the destination (e.g. user reinstalled
	// from /plugin install), back it up before swapping in. Easier to recover
	// from a botched promote than to merge two trees.
	backup := finalDir + ".bak"
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(finalDir); err == nil {
		if err := os.Rename(finalDir, backup); err != nil {
			os.RemoveAll(tmpDir)
			return fmt.Errorf("backup existing cache: %w", err)
		}
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		// Try to put the backup back so the user isn't left empty-handed.
		_ = os.Rename(backup, finalDir)
		return fmt.Errorf("promote tmp to final: %w", err)
	}
	// Successful swap: drop the backup. (RemoveAll is best-effort; leaving
	// .bak is harmless if cleanup fails — it's just disk usage.)
	_ = os.RemoveAll(backup)

	// Resolve the git commit sha of the mirror so the registry entry matches
	// what Claude Code would have written via /plugin install. Best-effort:
	// failure here doesn't block — Claude Code reads the field but doesn't
	// crash when it's absent.
	sha := mirrorHeadSha(mirrorDir)

	// Rewrite the registry. Single-plugin scope: only mutate our key, leave
	// every other plugin's entries untouched.
	now := time.Now().UTC().Format(time.RFC3339)
	entry := pluginRegistryEntry{
		Scope:        "user",
		InstallPath:  finalDir,
		Version:      version,
		InstalledAt:  now,
		LastUpdated:  now,
		GitCommitSha: sha,
	}
	// Preserve InstalledAt from any existing entry so the timeline doesn't
	// reset on every auto-update.
	if existing := findExistingEntry(doc, pluginRegistryKey); existing != nil {
		entry.InstalledAt = existing.InstalledAt
	}
	if err := writePluginRegistry(registryPath, doc, entry); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	return nil
}

// pluginRegistryDocument mirrors the parts of installed_plugins.json we
// understand. Other top-level fields are preserved via the catch-all map.
type pluginRegistryDocument struct {
	Version int                              `json:"version"`
	Plugins map[string][]pluginRegistryEntry `json:"plugins"`
	// extra holds any future top-level fields we don't recognize; we copy
	// them through verbatim on rewrite so Claude Code's schema growth
	// doesn't lose user state.
	extra map[string]json.RawMessage
}

type pluginRegistryEntry struct {
	Scope        string `json:"scope"`
	InstallPath  string `json:"installPath"`
	Version      string `json:"version"`
	InstalledAt  string `json:"installedAt"`
	LastUpdated  string `json:"lastUpdated"`
	GitCommitSha string `json:"gitCommitSha,omitempty"`
}

func loadPluginRegistry(path string) (*pluginRegistryDocument, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("parse top-level: %w", err)
	}
	doc := &pluginRegistryDocument{
		Plugins: map[string][]pluginRegistryEntry{},
		extra:   map[string]json.RawMessage{},
	}
	for k, v := range fields {
		switch k {
		case "version":
			_ = json.Unmarshal(v, &doc.Version)
		case "plugins":
			if err := json.Unmarshal(v, &doc.Plugins); err != nil {
				return nil, fmt.Errorf("parse plugins: %w", err)
			}
		default:
			doc.extra[k] = v
		}
	}
	return doc, nil
}

func findExistingEntry(doc *pluginRegistryDocument, key string) *pluginRegistryEntry {
	entries, ok := doc.Plugins[key]
	if !ok || len(entries) == 0 {
		return nil
	}
	// Pick the most recently installed entry to inherit InstalledAt from.
	// Claude Code's registry can have multiple entries (different scopes);
	// our scope is "user" so we prefer that one.
	for i := range entries {
		if entries[i].Scope == "user" {
			return &entries[i]
		}
	}
	return &entries[0]
}

// writePluginRegistry replaces only the entry keyed by pluginRegistryKey,
// preserves every other plugin's data, and writes back atomically via
// tmp+rename. Top-level fields we don't model are passed through unchanged.
func writePluginRegistry(path string, doc *pluginRegistryDocument, entry pluginRegistryEntry) error {
	doc.Plugins[pluginRegistryKey] = []pluginRegistryEntry{entry}

	out := make(map[string]json.RawMessage, len(doc.extra)+2)
	for k, v := range doc.extra {
		out[k] = v
	}
	versionRaw, _ := json.Marshal(doc.Version)
	out["version"] = versionRaw
	pluginsRaw, err := json.MarshalIndent(doc.Plugins, "", "  ")
	if err != nil {
		return err
	}
	out["plugins"] = pluginsRaw

	final, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	final = append(final, '\n')

	tmp := path + ".anchored.tmp"
	if err := os.WriteFile(tmp, final, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// mirrorHeadSha returns the HEAD commit sha of the marketplace mirror, or ""
// if git isn't available / the mirror isn't a repo. Used to populate the
// gitCommitSha field that Claude Code records on /plugin install.
func mirrorHeadSha(mirrorDir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = mirrorDir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// copyDirExcludingGit walks src and copies every entry into dst, skipping
// the .git tree (we don't need it, and it can be sizeable). Symlinks are
// recreated as symlinks; regular files are copied byte-for-byte with the
// source mode bits preserved.
func copyDirExcludingGit(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// Skip .git entirely; it's bookkeeping for the mirror, not the cache.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, target)
		default:
			return copyFileWithMode(path, target, info.Mode().Perm())
		}
	})
}

// rewriteInstalledHookCommands rewrites the "anchored" binary token in every
// "command" string of the given hooks.json to the resolved absolute binary
// path (anchoredBinaryPath). It is a no-op when the file doesn't exist, and
// logs-and-continues on any parse/write error — hooks.json is optional
// tooling, and a malformed or unreadable copy must never break the plugin
// install.
func rewriteInstalledHookCommands(hooksPath string) {
	data, err := os.ReadFile(hooksPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("read hooks.json for command rewrite", "path", hooksPath, "error", err)
		}
		return
	}

	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		slog.Warn("parse hooks.json for command rewrite", "path", hooksPath, "error", err)
		return
	}

	rewriteAnchoredCommands(doc, anchoredBinaryPath())

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		slog.Warn("marshal rewritten hooks.json", "path", hooksPath, "error", err)
		return
	}
	if err := os.WriteFile(hooksPath, append(out, '\n'), 0o644); err != nil {
		slog.Warn("write rewritten hooks.json", "path", hooksPath, "error", err)
	}
}

// rewriteAnchoredCommands walks an arbitrary decoded JSON value looking for
// "command" string fields that invoke the bare "anchored" binary, rewriting
// just the binary token in place so trailing args (e.g. "hook stop") survive
// untouched. Recurses through maps and slices to handle hooks.json's nested
// event -> matcher -> hooks[] shape without assuming a fixed structure.
func rewriteAnchoredCommands(node any, bin string) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "command" {
				if s, ok := val.(string); ok && strings.HasPrefix(s, "anchored ") {
					v[k] = bin + strings.TrimPrefix(s, "anchored")
					continue
				}
			}
			rewriteAnchoredCommands(val, bin)
		}
	case []any:
		for _, item := range v {
			rewriteAnchoredCommands(item, bin)
		}
	}
}

func copyFileWithMode(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pluginRegistryPath returns the canonical path Claude Code uses, with HOME
// override support for tests. Tests can swap HOME via t.Setenv.
func pluginRegistryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
}
