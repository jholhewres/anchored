package main

import (
	"net/http"
	"os"
	"strings"
)

// handleTokens surfaces the v0.13 recall telemetry (recall_logs) in the
// dashboard: how many context tokens anchored injected vs. the static-context
// baseline over the last 7 days. Reuses the same aggregation as
// `anchored stats --tokens`.
func (a *dashboardAPI) handleTokens(w http.ResponseWriter, r *http.Request) {
	s, err := queryRecallSummary(a.db, 7)
	if err != nil {
		dashWriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{
		"days":            7,
		"injections":      s.Injections,
		"injected_tokens": s.InjectedTokens,
		"baseline_tokens": s.BaselineTokens,
		"savings_pct":     s.SavingsPct(),
	})
}

// hostConnection is the per-host status reported by /api/connections.
type hostConnection struct {
	Name       string `json:"name"`
	Installed  bool   `json:"installed"`
	Registered bool   `json:"registered"`
}

// knownConnectionHosts is the set of hosts anchored can register into via
// `anchored init --tool <name>`, shown in the dashboard's Connections view.
var knownConnectionHosts = []string{
	"claude-code", "cursor", "opencode", "agy", "gemini", "windsurf",
	"cline", "vscode", "codex", "devin",
	"openclaw", "hermes", "devclaw", "gatorclaw", "supergator",
}

// handleConnections reports, for each known host, whether the tool is installed
// and whether anchored is registered in its MCP config. Read-only: it inspects
// config files on disk, never modifies them.
func (a *dashboardAPI) handleConnections(w http.ResponseWriter, r *http.Request) {
	cwd, _ := os.Getwd()
	out := make([]hostConnection, 0, len(knownConnectionHosts))
	for _, h := range knownConnectionHosts {
		out = append(out, hostConnection{
			Name:       h,
			Installed:  isToolInstalled(h, cwd),
			Registered: isAnchoredRegistered(h, cwd),
		})
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"hosts": out})
}

// isAnchoredRegistered reports whether the host's MCP config already contains an
// anchored server entry. Best-effort and read-only — any read/parse error is
// treated as "not registered" rather than surfaced.
func isAnchoredRegistered(t, cwd string) bool {
	path := getToolMCPPath(t, cwd)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	// A substring check is sufficient here: every registration writes a server
	// keyed or named "anchored", and this view is advisory (the source of truth
	// is `anchored init`). Avoids format-specific parsing across JSON/YAML/TOML.
	return containsAnchoredEntry(string(data))
}

func containsAnchoredEntry(s string) bool {
	for _, marker := range []string{`"anchored"`, "name: anchored", "[mcp_servers.anchored]", "anchored:"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
