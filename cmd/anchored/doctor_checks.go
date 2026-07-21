package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/sync"
)

// probeTimeout bounds every per-remote network probe so a dead server cannot
// stall the whole doctor run. Var (not const) so tests can shrink it.
var probeTimeout = 3 * time.Second

// checkResult is one doctor finding. The JSON shape ({name, status, detail,
// fix_command}) is a stable contract consumed by scripts and the e2e suite.
type checkResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // "ok" | "failed" | "skipped"
	Detail     string `json:"detail,omitempty"`
	FixCommand string `json:"fix_command,omitempty"`
	critical   bool
}

var (
	doctorChecks   []checkResult
	doctorJSONMode bool
)

// recordCheck appends a finding and, outside JSON mode, prints it in the
// human format doctor has always used.
func recordCheck(status, name, detail, fix string, critical bool) {
	doctorChecks = append(doctorChecks, checkResult{
		Name: name, Status: status, Detail: detail, FixCommand: fix, critical: critical,
	})
	if doctorJSONMode {
		return
	}
	mark := "[ ]"
	switch status {
	case "ok":
		mark = "[x]"
	case "skipped":
		mark = "[-]"
	case "warn":
		mark = "[!]"
	}
	fmt.Printf("%s %s", mark, name)
	if detail != "" {
		fmt.Printf(" — %s", detail)
	}
	fmt.Println()
	if (status == "failed" || status == "warn") && fix != "" {
		fmt.Printf("    → %s\n", fix)
	}
}

// finishDoctor emits the JSON document in --json mode and exits non-zero when
// any critical check failed. Critical = config load, database open, remote
// connectivity/auth for configured remotes. Informational checks (MCP host
// registrations, identity probe, plugin drift) never fail the run.
func finishDoctor() {
	exitCode := 0
	for _, c := range doctorChecks {
		if c.Status == "failed" && c.critical {
			exitCode = 1
			break
		}
	}
	if doctorJSONMode {
		out := struct {
			Version string        `json:"version"`
			Checks  []checkResult `json:"checks"`
		}{Version: Version, Checks: doctorChecks}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	} else {
		fmt.Println()
	}
	os.Exit(exitCode)
}

// keyPrefix returns at most the first 8 characters of an API key, masking
// anything shorter outright. Doctor must never print a full key — 8 chars is
// enough to tell keys apart without donating entropy to an attacker.
func keyPrefix(key string) string {
	const show = 8
	if len(key) <= show {
		return strings.Repeat("*", len(key))
	}
	return key[:show] + "…"
}

// sanitizeURL strips any userinfo (user:pass@) from a URL before it reaches
// terminal or JSON output — credentials embedded in server URLs must never
// be echoed back.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	return u.String()
}

// remoteProbe is the outcome of probing one remote's health + auth.
type remoteProbe struct {
	Class   string // "ok" | "dns" | "tls" | "timeout" | "auth" | "unreachable" | "http_<code>"
	Latency time.Duration
	Version string
}

// classifyProbeErr maps a transport error to an actionable class. Order
// matters: timeouts mention contexts, TLS errors mention x509 — check the
// specific shapes before falling back to string sniffing.
func classifyProbeErr(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	msg := err.Error()
	if strings.Contains(msg, "x509") || strings.Contains(msg, "tls:") || strings.Contains(msg, "certificate") {
		return "tls"
	}
	return "unreachable"
}

// probeRemote checks one remote in two steps: GET /v1/health (connectivity,
// latency, server version — unauthenticated) then GET /v1/me with the API key
// (auth validity). Any failure short-circuits with its class. Redirects are
// never followed: the probe endpoints don't redirect, and following one could
// hand the Bearer token to a different host.
func probeRemote(ctx context.Context, entry config.RemoteEntry) remoteProbe {
	client := &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	base := strings.TrimRight(entry.ServerURL, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/health", nil)
	if err != nil {
		return remoteProbe{Class: "unreachable"}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return remoteProbe{Class: classifyProbeErr(err)}
	}
	defer resp.Body.Close()
	latency := time.Since(start)
	var health struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&health)
	if resp.StatusCode != http.StatusOK {
		return remoteProbe{Class: fmt.Sprintf("http_%d", resp.StatusCode), Latency: latency}
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/me", nil)
	if err != nil {
		return remoteProbe{Class: "unreachable", Latency: latency, Version: health.Version}
	}
	req2.Header.Set("Authorization", "Bearer "+entry.APIKey)
	resp2, err := client.Do(req2)
	if err != nil {
		return remoteProbe{Class: classifyProbeErr(err), Latency: latency, Version: health.Version}
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusUnauthorized || resp2.StatusCode == http.StatusForbidden {
		return remoteProbe{Class: "auth", Latency: latency, Version: health.Version}
	}
	if resp2.StatusCode != http.StatusOK {
		return remoteProbe{Class: fmt.Sprintf("http_%d", resp2.StatusCode), Latency: latency, Version: health.Version}
	}
	return remoteProbe{Class: "ok", Latency: latency, Version: health.Version}
}

// remoteFix maps a probe class to the most likely fix command/action.
func remoteFix(class string, entry config.RemoteEntry) string {
	switch class {
	case "dns":
		return "hostname does not resolve — check the server URL in ~/.anchored/config.yaml and your network/VPN/DNS (on WSL check /etc/resolv.conf)"
	case "tls":
		return "TLS verification failed — update CA certificates (e.g. 'sudo apt install --reinstall ca-certificates') or check for a corporate proxy"
	case "timeout":
		return fmt.Sprintf("no response within %s — check firewall rules and that the server is running (curl -v %s/v1/health)", probeTimeout, strings.TrimRight(sanitizeURL(entry.ServerURL), "/"))
	case "auth":
		return fmt.Sprintf("API key %s rejected — generate a new key in the panel (API Keys) and update ~/.anchored/config.yaml", keyPrefix(entry.APIKey))
	default:
		return fmt.Sprintf("connection failed — verify the URL and try: curl -v %s/v1/health", strings.TrimRight(sanitizeURL(entry.ServerURL), "/"))
	}
}

// sortedRemoteNames returns the configured remote names, default first, then
// alphabetical — the same priority order sync resolution uses.
func sortedRemoteNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Remotes))
	for name := range cfg.Remotes {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		di, dj := cfg.Remotes[names[i]].Default, cfg.Remotes[names[j]].Default
		if di != dj {
			return di
		}
		return names[i] < names[j]
	})
	return names
}

// checkRemoteConnectivity probes every configured remote and reports
// connectivity, latency, server version and auth validity. Failures here are
// critical: a configured-but-unreachable remote means sync is silently dead.
// Returns whether at least one remote answered (gates the identity probe).
func checkRemoteConnectivity(cfg *config.Config) bool {
	if len(cfg.Remotes) == 0 {
		recordCheck("ok", "remotes: none configured (local-only mode)", "", "", false)
		return false
	}
	anyReachable := false
	for _, name := range sortedRemoteNames(cfg) {
		entry := cfg.Remotes[name]
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		probe := probeRemote(ctx, entry)
		cancel()

		label := fmt.Sprintf("remote %q (%s)", name, sanitizeURL(entry.ServerURL))
		switch probe.Class {
		case "ok":
			anyReachable = true
			detail := fmt.Sprintf("ok · %dms · server %s · key %s",
				probe.Latency.Milliseconds(), probe.Version, keyPrefix(entry.APIKey))
			recordCheck("ok", label, detail, "", false)
		case "auth":
			anyReachable = true
			recordCheck("failed", label,
				fmt.Sprintf("reachable (server %s) but the API key was rejected", probe.Version),
				remoteFix("auth", entry), true)
		default:
			recordCheck("failed", label, "connectivity failed: "+probe.Class,
				remoteFix(probe.Class, entry), true)
		}
	}
	return anyReachable
}

// checkRemoteConfigSanity validates the routing config: exactly one resolvable
// default, valid path globs, and the effective auto-sync per remote.
func checkRemoteConfigSanity(cfg *config.Config) {
	if len(cfg.Remotes) == 0 {
		return
	}

	var defaults []string
	for _, name := range sortedRemoteNames(cfg) {
		entry := cfg.Remotes[name]
		if entry.Default {
			defaults = append(defaults, name)
		}
		for _, pattern := range entry.Paths {
			if _, err := path.Match(pattern, "probe"); err != nil {
				recordCheck("failed", fmt.Sprintf("remote %q path pattern %q", name, pattern),
					"invalid glob: "+err.Error(),
					"fix the pattern in ~/.anchored/config.yaml (path.Match syntax: * does not cross /)", false)
			}
		}
		mode := "auto-sync on"
		if !entry.AutoSyncEnabled() {
			mode = "auto-sync off"
		}
		recordCheck("ok", fmt.Sprintf("remote %q routing", name),
			fmt.Sprintf("%s · %d path pattern(s) · default=%v", mode, len(entry.Paths), entry.Default), "", false)
	}

	switch len(defaults) {
	case 0:
		recordCheck("failed", "default remote", "no remote has default: true — saves outside configured paths will stay local",
			"set 'default: true' on one remote in ~/.anchored/config.yaml (or re-run 'anchored remote configure')", false)
	case 1:
		recordCheck("ok", "default remote: "+defaults[0], "", "", false)
	default:
		recordCheck("failed", "default remote", "multiple remotes marked default: "+strings.Join(defaults, ", "),
			"keep 'default: true' on exactly one remote in ~/.anchored/config.yaml", false)
	}
}

// checkProjectIdentity derives the cwd repo's remote keys and probes which
// configured remote knows them. Never critical, and skipped (not failed) when
// the network is down — identity is a routing question, not a health one.
func checkProjectIdentity(cfg *config.Config, cwd string, anyRemoteReachable bool) {
	if len(cfg.Remotes) == 0 {
		return
	}
	origin := gitOriginURL(cwd)
	if origin == "" {
		recordCheck("skipped", "project identity", "cwd is not a git repo with an 'origin' remote", "", false)
		return
	}
	if !anyRemoteReachable {
		recordCheck("skipped", "project identity", "no remote reachable — cannot probe (fix connectivity first)", "", false)
		return
	}

	canonicalKey := project.DeriveRemoteKeyFromURL(origin)
	legacyKey := project.DeriveLegacyRemoteKeyFromURL(origin)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	target, projectID, matchedKey := sync.ResolveProjectAcrossRemotes(ctx, cfg, cwd, "cli", canonicalKey, legacyKey)
	if target != nil && projectID != "" {
		recordCheck("ok", "project identity",
			fmt.Sprintf("%s → remote %q project %s (key %s)", origin, target.Name, projectID, matchedKey), "", false)
		return
	}
	recordCheck("failed", "project identity",
		fmt.Sprintf("no configured remote has a project for %s (keys tried: %s, %s)", origin, canonicalKey, legacyKey),
		"create the project in the panel with Repository URL "+origin+", or link an existing one: anchored remote link <slug> --remote <name>", false)
}

// checkPluginDrift reports when the Claude Code plugin mirror/cache lag the
// binary — stale hooks are a common "it works in CLI but not in the IDE".
func checkPluginDrift(cfg *config.Config) {
	drift := detectPluginDrift(cfg, Version)
	if drift.BinaryVersion == "" || drift.BinaryVersion == "dev" {
		return
	}
	if drift.MirrorVersion == "" && drift.CacheVersion == "" {
		recordCheck("skipped", "Claude Code plugin", "plugin not installed (mirror/cache absent)", "", false)
		return
	}
	// The plugin is versioned independently from the binary; a healthy install
	// has the cache matching the marketplace mirror. Report drift only when the
	// cache actually trails the mirror — not when the binary merely out-versions
	// the plugin, which is the steady state and was a permanent false positive.
	cacheVer := drift.CacheVersion
	if cacheVer == "" {
		cacheVer = "absent"
	}
	if !drift.CacheBehind {
		recordCheck("ok", fmt.Sprintf("Claude Code plugin up to date (mirror %s, cache %s)",
			drift.MirrorVersion, cacheVer), "", "", false)
		return
	}
	recordCheck("failed", "Claude Code plugin",
		fmt.Sprintf("plugin cache lags the marketplace mirror (mirror %s, cache %s) — hooks may be stale",
			drift.MirrorVersion, cacheVer),
		"run `/plugin install anchored@anchored` then restart Claude Code", false)
}

// --- Cursor activation (S1.6) ---

// requiredCursorHookEvents are the Cursor hook events anchored's activation
// depends on: beforeShellExecution/afterFileEdit for capture, stop for
// session close. beforeMCPExecution and beforeSubmitPrompt may also be wired
// but aren't required for the doctor check to pass.
var requiredCursorHookEvents = []string{"beforeShellExecution", "afterFileEdit", "stop"}

type cursorHooksDoc struct {
	Hooks map[string][]struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// anchoredHookCommandToken reports whether command invokes an anchored hook
// subcommand ("<bin> hook <sub> ...") and, if so, returns the binary token
// (fields[0]) as written in the config. Matching is on the base name only
// (ignoring any path prefix) so both "anchored hook stop" and
// "/home/x/.anchored/bin/anchored hook stop" match.
func anchoredHookCommandToken(command string) (string, bool) {
	fields := strings.Fields(command)
	if len(fields) < 2 {
		return "", false
	}
	if filepath.Base(fields[0]) != "anchored" || fields[1] != "hook" {
		return "", false
	}
	return fields[0], true
}

// checkCursorActivation runs the Cursor hooks.json + rule checks, taking the
// user's home directory as a parameter so tests can point it at a t.TempDir()
// fixture. When ~/.cursor doesn't exist, Cursor isn't installed on this
// machine and the checks are skipped with no output at all (not even
// "skipped") to avoid cluttering doctor output for users who don't use it.
func checkCursorActivation(home string) {
	cursorDir := filepath.Join(home, ".cursor")
	if _, err := os.Stat(cursorDir); err != nil {
		return
	}
	checkCursorHooks(filepath.Join(cursorDir, "hooks.json"))
	checkCursorRule(filepath.Join(cursorDir, "rules", "anchored.mdc"))
}

func checkCursorHooks(path string) {
	const remedy = "anchored init --tool cursor"

	data, err := os.ReadFile(path)
	if err != nil {
		printCheck(false, "Cursor hooks.json wires anchored hooks", "not found: "+path, remedy)
		return
	}

	var doc cursorHooksDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		printCheck(false, "Cursor hooks.json wires anchored hooks", "invalid JSON: "+err.Error(), remedy)
		return
	}

	haveEvent := map[string]bool{}
	tokenSet := map[string]bool{}
	for event, entries := range doc.Hooks {
		for _, entry := range entries {
			if tok, ok := anchoredHookCommandToken(entry.Command); ok {
				haveEvent[event] = true
				tokenSet[tok] = true
			}
		}
	}

	var missing []string
	for _, event := range requiredCursorHookEvents {
		if !haveEvent[event] {
			missing = append(missing, event)
		}
	}

	if len(missing) > 0 {
		printCheck(false, "Cursor hooks.json wires anchored hooks",
			"missing for event(s): "+strings.Join(missing, ", "), remedy)
	} else {
		printCheck(true, "Cursor hooks.json wires beforeShellExecution/afterFileEdit/stop", "", "")
	}

	if len(tokenSet) == 0 {
		return
	}
	var pathDependent []string
	for tok := range tokenSet {
		info, statErr := os.Stat(tok)
		if !filepath.IsAbs(tok) || statErr != nil || info.IsDir() {
			pathDependent = append(pathDependent, tok)
		}
	}
	sort.Strings(pathDependent)
	if len(pathDependent) > 0 {
		recordCheck("warn", "Cursor hook commands use an absolute path",
			"hook command is PATH-dependent: "+strings.Join(pathDependent, ", "), remedy, false)
	} else {
		printCheck(true, "Cursor hook commands use an absolute path", "", "")
	}
}

func checkCursorRule(path string) {
	if _, err := os.Stat(path); err != nil {
		printCheck(false, "Cursor rule ~/.cursor/rules/anchored.mdc present", "missing", "anchored init --tool cursor")
		return
	}
	printCheck(true, "Cursor rule ~/.cursor/rules/anchored.mdc present", "", "")
}

// --- Embedding coverage (S2.4) ---

// embeddingCoverageThreshold is the minimum percentage of active memories
// that must carry an embedding before recall quality is considered healthy.
const embeddingCoverageThreshold = 80

// embeddingCoveragePercent returns the whole-number percentage embedded,
// treating an empty corpus as fully covered (there's nothing to embed).
func embeddingCoveragePercent(embedded, total int) int {
	if total == 0 {
		return 100
	}
	return embedded * 100 / total
}

// embeddingCoverageOK reports whether coverage meets embeddingCoverageThreshold.
func embeddingCoverageOK(embedded, total int) bool {
	return embeddingCoveragePercent(embedded, total) >= embeddingCoverageThreshold
}

// --- Maintenance timer (S2.4, Linux only) ---

// checkMaintenanceTimer verifies the anchored-maintenance systemd --user
// timer (installed by `anchored maintenance install`, see maintenance.go) is
// present and enabled. Skipped silently on non-Linux platforms, where the
// timer isn't offered at all.
func checkMaintenanceTimer(home string) {
	if runtime.GOOS != "linux" {
		return
	}
	checkMaintenanceTimerAt(filepath.Join(home, ".config", "systemd", "user"))
}

// checkMaintenanceTimerAt takes the systemd --user unit directory as a
// parameter (mirroring maintenanceUnitDir in maintenance.go) so tests can
// point it at a t.TempDir() fixture without touching the real ~/.config.
func checkMaintenanceTimerAt(unitDir string) {
	const remedy = "anchored maintenance install"
	timerPath := filepath.Join(unitDir, maintenanceUnitName+".timer")
	if _, err := os.Stat(timerPath); err != nil {
		recordCheck("warn", "anchored-maintenance systemd timer", "not installed", remedy, false)
		return
	}

	out, err := exec.Command("systemctl", "--user", "is-enabled", maintenanceUnitName+".timer").Output()
	if err != nil || strings.TrimSpace(string(out)) != "enabled" {
		recordCheck("warn", "anchored-maintenance systemd timer", "installed but not enabled", remedy, false)
		return
	}
	printCheck(true, "anchored-maintenance systemd timer installed and enabled", "", "")
}

// --- Debug log observability (S3.3) ---

// checkDebugLog reports on the optional NDJSON hook/MCP event log
// (pkg/debuglog). It intentionally re-resolves enabled/path itself instead of
// calling debuglog.Open, which would create the log file as a side effect of
// running doctor and defeat the staleness check on a first run.
func checkDebugLog(cfg *config.Config, home string) {
	enabled := cfg.Debug.Enabled
	if v, ok := os.LookupEnv("ANCHORED_DEBUG"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			enabled = true
		case "0", "false", "off", "no", "":
			enabled = false
		}
	}
	if !enabled {
		recordCheck("ok", "hook observability", "debug log disabled — enable debug.enabled in config.yaml to troubleshoot hooks", "", false)
		return
	}

	path := cfg.Debug.Path
	if v := strings.TrimSpace(os.Getenv("ANCHORED_DEBUG_PATH")); v != "" {
		path = v
	}
	switch {
	case path == "":
		path = filepath.Join(home, ".anchored", "debug.log")
	case strings.HasPrefix(path, "~/"):
		path = filepath.Join(home, path[2:])
	}

	info, err := os.Stat(path)
	if err != nil {
		recordCheck("warn", "hook observability", "debug enabled but log file not found: "+path, "", false)
		return
	}
	age := time.Since(info.ModTime())
	if age > 7*24*time.Hour {
		recordCheck("warn", "hook observability",
			fmt.Sprintf("debug log stale (last write %s ago) — hooks may be failing silently", age.Round(time.Hour)),
			"", false)
		return
	}
	recordCheck("ok", "hook observability",
		fmt.Sprintf("debug log fresh (last write %s ago)", age.Round(time.Minute)), "", false)
}

// --- Model label hygiene ---

// modelDirName returns the subdirectory under modelDir that actually holds
// the ONNX model (the same one checkONNX treats as "the" model — the first
// entry containing model.onnx), or "" when none is found.
func modelDirName(modelDir string) string {
	entries, err := os.ReadDir(modelDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(modelDir, e.Name(), "model.onnx")); err == nil {
			return e.Name()
		}
	}
	return ""
}

// checkModelLabel warns when config's embedding.model label doesn't match
// the model directory actually on disk. This is cosmetic, not a vector-space
// risk: the loader keys off ModelDir only (pkg/memory/service.go), so a
// mismatch means the config is misleading, not that embeddings are wrong.
func checkModelLabel(label, modelDir string) {
	dirName := modelDirName(modelDir)
	if dirName == "" || label == "" {
		return // ONNX check above already reports a missing/absent model
	}
	if label == dirName {
		printCheck(true, fmt.Sprintf("embedding.model label matches model_dir (%s)", label), "", "")
		return
	}
	recordCheck("warn", "embedding.model label matches model_dir",
		fmt.Sprintf("config label %q, disk has %q", label, dirName),
		"update embedding.model in config.yaml (label only — loader uses model_dir)", false)
}
