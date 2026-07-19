package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
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
// Purely project-scoped hosts (e.g. vscode, whose config lives at
// cwd/.vscode/mcp.json) are omitted: the dashboard server can't know the user's
// project directory, so its registration status can't be reported reliably.
var knownConnectionHosts = []string{
	"claude-code", "cursor", "opencode", "agy", "gemini", "windsurf",
	"cline", "codex", "devin",
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

// isAnchoredRegistered reports whether the host's MCP config actually contains
// an anchored server entry (not just a stray mention of the word). Read-only;
// any read/parse error is treated as "not registered". Parses by the host's
// config format so an unrelated "anchored" label can't false-positive.
func isAnchoredRegistered(t, cwd string) bool {
	path := getToolMCPPath(t, cwd)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	switch getToolMCPConfig(t).format {
	case "yaml-map":
		return yamlMapHasAnchored(data, "mcp_servers")
	case "yaml-array":
		return yamlArrayHasAnchored(data, "mcp")
	default:
		if getToolMCPConfig(t).isTOML {
			return strings.Contains(string(data), "[mcp_servers.anchored]")
		}
		return jsonMapHasAnchored(data, getToolMCPConfig(t).rootKey)
	}
}

// jsonMapHasAnchored reports whether cfg[rootKey] is a map containing an
// "anchored" server entry.
func jsonMapHasAnchored(data []byte, rootKey string) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(doc[rootKey], &servers) != nil {
		return false
	}
	_, ok := servers["anchored"]
	return ok
}

func yamlMapHasAnchored(data []byte, rootKey string) bool {
	doc := map[string]any{}
	if yaml.Unmarshal(data, &doc) != nil {
		return false
	}
	servers, _ := doc[rootKey].(map[string]any)
	_, ok := servers["anchored"]
	return ok
}

func yamlArrayHasAnchored(data []byte, rootKey string) bool {
	doc := map[string]any{}
	if yaml.Unmarshal(data, &doc) != nil {
		return false
	}
	section, _ := doc[rootKey].(map[string]any)
	list, _ := section["servers"].([]any)
	for _, it := range list {
		if e, _ := it.(map[string]any); e != nil {
			if name, _ := e["name"].(string); name == "anchored" {
				return true
			}
		}
	}
	return false
}
