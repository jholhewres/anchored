//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

// `anchored config` and `anchored remote` rewrite the config, which carries
// remote API keys. os.WriteFile only applies its mode to new files, so an
// existing 0664 config must be tightened explicitly.
func TestWriteConfigFileKeepsTheConfigPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("remote:\n  api_key: x\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	writeConfigFile(path, config.Defaults())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o, want 600", info.Mode().Perm())
	}
}

// Another host's config (Hermes, devclaw…) keeps its own mode; its backup
// inherits that mode instead of a world-readable 0644.
func TestForeignConfigBackupInheritsTheOriginalMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hermes.yaml")
	if err := os.WriteFile(path, []byte("memory: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, _ := os.ReadFile(path)
	if err := writeYAMLDoc(path, map[string]any{"memory": map[string]any{"provider": "anchored"}}, prev); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + ".bak"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want the original's 600", filepath.Base(p), info.Mode().Perm())
		}
	}
}

// Every init path that backs up another tool's config (Codex TOML, Cursor
// hooks, plugin files) must keep the backup as private as the original, and a
// world-readable .bak left by an older version is tightened.
func TestWriteBackupFileInheritsTheOriginalMode(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(orig, []byte("token = \"x\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(orig, 0o640); err != nil {
		t.Fatal(err)
	}
	// A backup an older version wrote world-readable.
	if err := os.WriteFile(orig+".bak", []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(orig+".bak", 0o644); err != nil {
		t.Fatal(err)
	}
	writeBackupFile(orig, []byte("token = \"x\"\n"))
	info, err := os.Stat(orig + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf(".bak mode = %o, want the original's 640", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(orig + ".bak"); string(raw) != "token = \"x\"\n" {
		t.Errorf(".bak content = %q", raw)
	}
}
