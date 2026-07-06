package main

import (
	"os"
	"path/filepath"
)

// osExecutable is a seam for tests: it lets a test simulate os.Executable()
// failing so the $HOME/.anchored/bin/anchored and bare "anchored" fallbacks
// in anchoredBinaryPath are reachable without depending on the real process
// environment (os.Executable() always succeeds under `go test`).
var osExecutable = os.Executable

// anchoredBinaryPath resolves the path to write into tool configs (MCP
// registration commands, hook commands) for the anchored binary. Writing an
// absolute path avoids PATH-dependent failures when a tool launches its MCP
// server or hooks outside a login shell (e.g. a GUI app launch that doesn't
// source ~/.profile).
//
// Preference order: the currently running executable, symlink-resolved so a
// wrapper/symlink path doesn't get baked in; then the stable install
// location at $HOME/.anchored/bin/anchored if it exists; then a bare
// "anchored", relying on PATH as a last resort.
func anchoredBinaryPath() string {
	if exe, err := osExecutable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved
		}
		return exe
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".anchored", "bin", "anchored")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "anchored"
}
