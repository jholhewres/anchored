package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/contextbudget"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/session"
)

const sessionStartQueryTimeout = 150 * time.Millisecond

// runHookSessionStart emits a Claude Code SessionStart hook payload that
// injects the anchored routing block plus a project-scoped recap of recent
// decisions / events. The shape `{hookSpecificOutput:{hookEventName,
// additionalContext}}` is the contract Claude Code reads to add a system
// reminder for the model. Cursor / OpenCode follow the same convention.
func runHookSessionStart(args []string) {
	fs := newFlagSet("hook sessionstart")
	sessionID := fs.String("session-id", "", "session identifier")
	configPath := fs.String("config", "", "path to config file")
	cwd := fs.String("cwd", "", "current working directory")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		dlog.Event("hook.sessionstart", map[string]any{"stage": "stdin_error", "error": err.Error()})
		emitSessionStart(mcp.AnchoredRoutingBlock)
		return
	}

	var input struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		slog.Debug("hook sessionstart: failed to parse stdin JSON", "error", err)
	}

	_ = sessionID

	cwdVal := *cwd
	if cwdVal == "" {
		cwdVal = input.Cwd
	}
	if cwdVal == "" {
		cwdVal = input.Directory
	}
	if cwdVal == "" {
		cwdVal = "."
	}

	resolvedSessionID := input.SessionID

	additional := mcp.AnchoredRoutingBlock

	// Plugin drift check: when the installed plugin cache is older than the
	// running binary, the user is missing hooks/skills from the new release.
	// We always notify; if config.Plugin.AutoUpdate is on we also fast-
	// forward the marketplace mirror + wipe the stale cache so Claude Code
	// reinstalls on its next launch.
	cfg, _ := loadConfig(*configPath)
	if cfg != nil {
		drift := detectPluginDrift(cfg, Version)
		if drift.HasDrift {
			if cfg.Plugin.AutoUpdate {
				drift = applyPluginAutoUpdate(drift)
			}
			if notice := renderPluginUpdateNotice(drift); notice != "" {
				additional += "\n\n" + notice
			}
			dlog.Event("hook.sessionstart", map[string]any{
				"stage":               "plugin_drift",
				"binary":              drift.BinaryVersion,
				"mirror":              drift.MirrorVersion,
				"cache":               drift.CacheVersion,
				"mirror_behind":       drift.MirrorBehind,
				"cache_behind":        drift.CacheBehind,
				"mirror_synced":       drift.SyncPerformed,
				"sync_error":          drift.SyncError,
				"cache_installed":     drift.CacheInstalled,
				"cache_install_error": drift.CacheInstallError,
				"marketplace_dir":     drift.MarketplaceDir,
				"cache_dir":           drift.CacheDir,
			})
		}
	}

	dlog.Event("hook.sessionstart", map[string]any{
		"stage":      "start",
		"session_id": input.SessionID,
		"cwd":        cwdVal,
		"input_len":  len(content),
		"input_head": debuglog.Snippet(string(content), 200),
	})

	hc, err := openHookContext(*configPath)
	if err != nil {
		dlog.Event("hook.sessionstart", map[string]any{"stage": "service_init_failed", "error": err.Error()})
		// Routing block alone is still useful even if the DB is unavailable.
		emitSessionStart(additional)
		return
	}
	defer hc.Close()

	projectID := hc.ResolveProject(cwdVal)
	ctx := context.Background()

	// Cold-start seeding: the first time a project is seen its store is empty,
	// so anchored_context/search return nothing and the tool reads as "not
	// working". Seed it from repo signal in a detached child so the session
	// never blocks. A5: tell the model it's happening so an empty first recall
	// isn't mistaken for a broken memory.
	autoBootstrap := cfg != nil && cfg.Plugin.AutoBootstrapEnabled()
	if maybeAutoBootstrap(hc, projectID, cwdVal, *configPath, autoBootstrap, dlog) {
		additional += "\n\n<anchored_bootstrap>New project detected — seeding memory from this repo (README, docs, project rules). Recall will fill in shortly.</anchored_bootstrap>"
	}

	// Register/resume this session so the cockpit can show it live. StartSession
	// itself is unchanged; provider/model come from the environment (this is
	// what distinguishes Claude Code on Anthropic from Claude Code + GLM).
	// Best-effort: a failure here never blocks the session start.
	if resolvedSessionID != "" {
		mgr := session.NewManager(hc.db, nil)
		if sid, sErr := mgr.StartSession(ctx, "claude-code", resolvedSessionID, projectID, cwdVal); sErr == nil {
			if provider, model := detectProvider(os.Getenv); provider != "" {
				_ = mgr.UpdateSessionMeta(ctx, sid, provider, model)
			}
		} else {
			dlog.Event("hook.sessionstart", map[string]any{"stage": "start_session_failed", "error": sErr.Error()})
		}
	}

	// When the resolved budget is 0 the user has opted out of the rich block;
	// fall back to the original plain format (RoutingBlock + events). Budget is
	// measured in approximate tokens (default 2000).
	budget := 2000
	if cfg != nil {
		budget = cfg.Plugin.SessionStartBudgetTokensResolved()
	}

	if budget == 0 {
		// Legacy path: plain recent_events block, no budgeter.
		appendLegacyRecentEvents(ctx, hc, projectID, &additional)
		dlog.Event("hook.sessionstart", map[string]any{
			"stage":      "emitted_legacy",
			"project_id": projectID,
		})
		emitSessionStart(additional)
		return
	}

	// Rich path: assemble tiers via contextbudget.
	tiers := buildSessionStartTiers(ctx, hc, resolvedSessionID, projectID, cwdVal)
	richBlock, dropped := contextbudget.Assemble(tiers, budget)

	if richBlock != "" {
		additional += "\n\n<anchored_context>\n" + richBlock + "\n</anchored_context>"
	}

	// Token telemetry for `anchored stats --tokens`: how many tokens this
	// injection cost vs. the static-context baseline it stands in for.
	// Best-effort and local; never blocks the hook.
	recordRecall(hc.db, projectID, "sessionstart",
		contextbudget.ApproxTokens(richBlock),
		projectBaselineTokens(hc.db, projectID, cwdVal, time.Now()))

	dlog.Event("hook.sessionstart", map[string]any{
		"stage":         "emitted",
		"project_id":    projectID,
		"context_bytes": len(richBlock),
		"dropped_items": dropped,
		"budget_tokens": budget,
	})
	emitSessionStart(additional)
}

// buildSessionStartTiers assembles the five tiers for the rich context block.
// Each tier is best-effort: any DB error causes that tier to be empty (fail-safe).
func buildSessionStartTiers(ctx context.Context, hc *HookContext, sessionID, projectID, cwd string) []contextbudget.Tier {
	queryCtx, cancel := context.WithTimeout(ctx, sessionStartQueryTimeout)
	defer cancel()

	// ── Tier 0: standing rules (user directives) ─────────────────────────────
	// First-party do/don't rules the user explicitly registered; injected
	// unranked at the top so they never compete with recalled data.
	ruleItems := queryDirectives(queryCtx, hc, projectID)

	// ── Tier 1: identity ────────────────────────────────────────────────────
	var identityItems []contextbudget.Item
	if id := readSessionIdentity(); id != "" {
		identityItems = []contextbudget.Item{{Text: id, Priority: 0}}
	}

	// ── Tier 2: decisions (pinned + decision/learning, top 5) ───────────────
	decisionItems := queryDecisions(queryCtx, hc, projectID)

	// ── Tier 3: task (active task thread + working set) ─────────────────────
	// The thread comes first: when the branch carries a ticket key, the
	// session is registered on the thread and a compact cross-repo block is
	// injected (what the same task touched in OTHER projects — references
	// only, never the other repos' full data).
	// queryCtx bounds the whole tier (upsert + cross-repo queries included):
	// a contended DB must degrade to an empty tier, never hang the hook.
	var taskItems []contextbudget.Item
	mgr := session.NewManager(hc.db, nil)
	if item, ok := taskThreadItem(queryCtx, hc, mgr, sessionID, projectID, cwd); ok {
		taskItems = append(taskItems, item)
	}
	if sessionID != "" {
		ws, err := mgr.GetWorkingSet(queryCtx, sessionID)
		if err == nil && ws != nil && !ws.Empty() {
			taskItems = append(taskItems, contextbudget.Item{Text: renderWorkingSetCompact(ws), Priority: 1})
		}
	}

	// ── Tier 4: recent events ────────────────────────────────────────────────
	eventItems := queryRecentEvents(queryCtx, hc, projectID)

	return []contextbudget.Tier{
		{Name: "standing_rules", Items: ruleItems, MinItems: 4},
		{Name: "identity", Items: identityItems, MinItems: 1},
		{Name: "decisions", Items: decisionItems, MinItems: 1},
		{Name: "task", Items: taskItems, MinItems: 0},
		{Name: "events", Items: eventItems, MinItems: 0},
	}
}

// taskThreadItem registers the session on the branch-inferred task thread and
// renders the cross-repo block: the thread's key/status/journal plus what the
// SAME task touched in OTHER projects (project names + their sessions' files,
// capped) — references only, advisory by design. Returns ok=false when the
// branch carries no ticket key or the thread is terminal.
func taskThreadItem(ctx context.Context, hc *HookContext, mgr *session.Manager, sessionID, projectID, cwd string) (contextbudget.Item, bool) {
	key := session.InferTaskKey(currentGitBranch(cwd))
	if key == "" {
		return contextbudget.Item{}, false
	}
	t, err := mgr.UpsertTaskThread(ctx, key, session.TaskThreadDelta{
		ProjectID: projectID,
		SessionID: sessionID,
	})
	if err != nil || t == nil || (t.Status != session.TaskStatusActive && t.Status != session.TaskStatusPaused) {
		return contextbudget.Item{}, false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<task_thread key=%q status=%q projects=\"%d\"", t.TaskKey, t.Status, len(t.ProjectIDs))
	if t.ExternalRef != "" {
		fmt.Fprintf(&b, " ref=%q", sessionEscapeAttr(t.ExternalRef))
	}
	b.WriteString(">")
	if len(t.Journal) > 0 {
		n := len(t.Journal)
		if n > 2 {
			n = 2
		}
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "<note>%s</note>", escapeText(sessionTruncateRunes(t.Journal[i], 160)))
		}
	}
	for _, line := range crossRepoLines(ctx, hc, t, projectID) {
		b.WriteString(line)
	}
	b.WriteString("</task_thread>")
	return contextbudget.Item{Text: b.String(), Priority: 0}, true
}

// crossRepoLines renders, for each OTHER project the thread touched, its name
// and the most recent files its sessions worked on (cap 6 files, 3 projects).
func crossRepoLines(ctx context.Context, hc *HookContext, t *session.TaskThread, currentProjectID string) []string {
	if len(t.SessionIDs) == 0 {
		return nil
	}
	var lines []string
	count := 0
	for _, pid := range t.ProjectIDs {
		if pid == currentProjectID || pid == "" {
			continue
		}
		if count >= 3 {
			break
		}
		var name string
		if err := hc.db.QueryRowContext(ctx,
			`SELECT name FROM projects WHERE id = ?`, pid).Scan(&name); err != nil {
			name = pid
		}
		var files string
		if err := hc.db.QueryRowContext(ctx, `
			SELECT files FROM working_sets
			WHERE project_id = ? AND session_id IN (`+jsonListPlaceholders(len(t.SessionIDs))+`)
			ORDER BY updated_at DESC LIMIT 1`,
			append([]any{pid}, toAnySlice(t.SessionIDs)...)...).Scan(&files); err != nil {
			slog.Debug("hook sessionstart: failed to query working set files", "project_id", pid, "error", err)
		}
		fileList := decodeFilesPreview(files, 6)
		line := fmt.Sprintf("<also_touched project=%q", sessionEscapeAttr(name))
		if fileList != "" {
			line += fmt.Sprintf(" files=%q", sessionEscapeAttr(fileList))
		}
		line += "/>"
		lines = append(lines, line)
		count++
	}
	return lines
}

func jsonListPlaceholders(n int) string {
	if n == 0 {
		return "''"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// decodeFilesPreview turns a JSON string list into "a, b, c" capped at max.
func decodeFilesPreview(raw string, max int) string {
	if raw == "" {
		return ""
	}
	var items []string
	if err := json.Unmarshal([]byte(raw), &items); err != nil || len(items) == 0 {
		return ""
	}
	if len(items) > max {
		items = items[:max]
	}
	return strings.Join(items, ", ")
}

// queryDirectives returns the user's standing rules: global directives plus
// the ones scoped to the current project, oldest first (stable order). Capped
// at 8; terse one-line rules are expected. Fail-safe: errors return nil.
func queryDirectives(ctx context.Context, hc *HookContext, projectID string) []contextbudget.Item {
	rows, err := hc.db.QueryContext(ctx, `
		SELECT content, COALESCE(project_id, '')
		FROM memories
		WHERE json_extract(metadata, '$.directive') = 1
		  AND deleted_at IS NULL
		  AND (project_id = '' OR project_id IS NULL OR project_id = ?)
		ORDER BY created_at ASC
		LIMIT 8`,
		projectID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []contextbudget.Item
	for rows.Next() {
		var content, pid string
		if err := rows.Scan(&content, &pid); err != nil {
			continue
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		scope := "user"
		if pid != "" {
			scope = "project"
		}
		text := fmt.Sprintf("<standing_rule scope=%q>%s</standing_rule>",
			scope, escapeText(content))
		items = append(items, contextbudget.Item{Text: text, Priority: len(items)})
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return items
}

// readSessionIdentity reads ~/.anchored/identity.md, capped at 600 runes.
func readSessionIdentity() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".anchored", "identity.md"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	return sessionTruncateRunes(s, 600)
}

// queryDecisions returns up to 5 decision/learning memories for the project,
// pinned first then by recency. Fail-safe: errors return nil.
func queryDecisions(ctx context.Context, hc *HookContext, projectID string) []contextbudget.Item {
	rows, err := hc.db.QueryContext(ctx, `
		SELECT content, category,
		       COALESCE(json_extract(metadata, '$.pinned'), 0) AS pinned
		FROM memories
		WHERE (project_id = ? OR project_id = '' OR project_id IS NULL)
		  AND deleted_at IS NULL
		  AND (COALESCE(json_extract(metadata, '$.pinned'), 0) = 1
		       OR category IN ('decision', 'learning'))
		  AND COALESCE(json_extract(metadata, '$.directive'), 0) != 1
		  AND COALESCE(json_extract(metadata, '$.curation_status'), 'ok')
		      NOT IN ('low_signal', 'near_duplicate', 'rejected')
		ORDER BY COALESCE(json_extract(metadata, '$.pinned'), 0) DESC, created_at DESC
		LIMIT 5`,
		projectID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []contextbudget.Item
	for rows.Next() {
		var content, category string
		var pinned int
		if err := rows.Scan(&content, &category, &pinned); err != nil {
			continue
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		text := fmt.Sprintf("<memory category=%q pinned=%d>%s</memory>",
			sessionEscapeAttr(category), pinned, escapeText(content))
		priority := 1
		if pinned == 1 {
			priority = 0
		}
		items = append(items, contextbudget.Item{Text: text, Priority: priority})
	}
	// A mid-iteration error (context deadline, I/O) leaves the set partial;
	// honour the fail-safe contract by treating it as empty rather than
	// presenting an incomplete block as complete.
	if err := rows.Err(); err != nil {
		return nil
	}
	return items
}

// queryRecentEvents returns up to 8 recent session events (priority <= 2).
// Fail-safe: errors return nil.
func queryRecentEvents(ctx context.Context, hc *HookContext, projectID string) []contextbudget.Item {
	rows, err := hc.db.QueryContext(ctx, `
		SELECT event_type, summary FROM session_events
		WHERE priority <= 2 AND (project_id = ? OR project_id = '')
		ORDER BY created_at DESC LIMIT 8`,
		projectID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []contextbudget.Item
	for rows.Next() {
		var eventType, summary string
		if err := rows.Scan(&eventType, &summary); err != nil {
			continue
		}
		if strings.TrimSpace(summary) == "" {
			continue
		}
		text := fmt.Sprintf("<event type=%q>%s</event>",
			sessionEscapeAttr(eventType), escapeText(summary))
		items = append(items, contextbudget.Item{Text: text, Priority: len(items)})
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return items
}

// renderWorkingSetCompact renders a single compact line from the working set.
func renderWorkingSetCompact(ws *session.WorkingSet) string {
	var parts []string
	if len(ws.Files) > 0 {
		parts = append(parts, "files="+strings.Join(ws.Files, ","))
	}
	if len(ws.Tests) > 0 {
		parts = append(parts, "tests="+strings.Join(ws.Tests, ","))
	}
	if len(ws.Symbols) > 0 {
		parts = append(parts, "symbols="+strings.Join(ws.Symbols, ","))
	}
	if ws.TopicKey != "" {
		parts = append(parts, "topic="+ws.TopicKey)
	}
	return "<working_set>" + escapeText(strings.Join(parts, " ")) + "</working_set>"
}

// appendLegacyRecentEvents appends the original plain recent_events block.
// Used when sessionstart_budget_bytes == 0 (opt-out of rich block).
func appendLegacyRecentEvents(ctx context.Context, hc *HookContext, projectID string, additional *string) {
	type recentEvent struct {
		EventType string
		Summary   string
	}
	var recent []recentEvent
	rows, err := hc.db.QueryContext(ctx,
		`SELECT event_type, summary FROM session_events
		 WHERE priority <= 2 AND (project_id = ? OR project_id = '')
		 ORDER BY created_at DESC LIMIT 8`,
		projectID,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e recentEvent
			if err := rows.Scan(&e.EventType, &e.Summary); err == nil && strings.TrimSpace(e.Summary) != "" {
				recent = append(recent, e)
			}
		}
	}

	if len(recent) > 0 {
		var sb strings.Builder
		sb.WriteString("\n\n<anchored_recent_events>\n")
		for _, e := range recent {
			fmt.Fprintf(&sb, "  <event type=%q>%s</event>\n", e.EventType, e.Summary)
		}
		sb.WriteString("</anchored_recent_events>")
		*additional += sb.String()
	}
}

// sessionTruncateRunes caps a string at max runes (not bytes), preserving UTF-8.
func sessionTruncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// sessionEscapeAttr escapes characters that would break XML double-quoted attrs.
func sessionEscapeAttr(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;",
		"\"", "&quot;", "\r", "&#xD;", "\n", "&#xA;", "\t", "&#x9;",
	)
	return r.Replace(s)
}

// maybeAutoBootstrap seeds a brand-new project's memory the first time it is
// seen. It runs only when enabled and the project's store is cold (zero
// memories), and it spawns `anchored bootstrap` as a fully detached child so
// the session hook never blocks on file reads, inserts, or embedding backfill.
// Returns true when a bootstrap was launched. Best-effort: any failure leaves
// the session start untouched.
func maybeAutoBootstrap(hc *HookContext, projectID, cwd, configPath string, enabled bool, dlog *debuglog.Logger) bool {
	if !enabled || projectID == "" {
		return false // disabled, or no git project to scope a bootstrap to
	}

	// Cold check: any existing memory means the project was already seen, so
	// never re-seed. content_hash dedup inside `bootstrap` covers the rare race
	// where two sessions both observe an empty store.
	ctx, cancel := context.WithTimeout(context.Background(), sessionStartQueryTimeout)
	defer cancel()
	var count int
	if err := hc.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM memories WHERE project_id = ? AND deleted_at IS NULL",
		projectID,
	).Scan(&count); err != nil || count > 0 {
		return false
	}

	self, err := os.Executable()
	if err != nil {
		dlog.Event("hook.sessionstart", map[string]any{"stage": "autobootstrap_no_executable", "error": err.Error()})
		return false
	}

	args := []string{"bootstrap", "--cwd", cwd}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	cmd := exec.Command(self, args...)
	// Detach: no inherited stdio, no Wait. The child opens its own DB handle
	// (WAL + busy_timeout) and exits independently of this short-lived hook.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		dlog.Event("hook.sessionstart", map[string]any{"stage": "autobootstrap_spawn_failed", "error": err.Error()})
		return false
	}
	_ = cmd.Process.Release()
	dlog.Event("hook.sessionstart", map[string]any{
		"stage": "autobootstrap_spawned", "project_id": projectID, "pid": cmd.Process.Pid,
	})
	return true
}

func emitSessionStart(additional string) {
	outputJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": additional,
		},
	})
}
