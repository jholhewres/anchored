package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/contextbudget"
	"unicode"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/hookroute"
	"github.com/jholhewres/anchored/pkg/intent"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
)

// preSearchTimeout caps how long we wait on the BM25 query so a slow DB
// never blocks the user's prompt from reaching the model. Pre-search is
// always best-effort: missing hits fall back to the routing block alone.
const preSearchTimeout = 150 * time.Millisecond

// kgQueryTimeout caps the KG lookup added in G2.
const kgQueryTimeout = 50 * time.Millisecond

// preSearchLimit is the max candidate rows from BM25; the budget decides
// how many survive into the rendered block. Raised from 3 → 5 in G2.
const preSearchLimit = 5

// recallMinTokens is the floor of meaningful tokens an unknown-intent prompt
// must have before we run retrieval. Trivial prompts ("oi", "hi", "thanks")
// sanitize below this and inject nothing but the reminder.
const recallMinTokens = 3

// anchorQueryCap is the total token cap for the expanded BM25 query.
// Anchors take precedence over free-text tokens when the cap is exceeded.
const anchorQueryCap = 24

// runHookUserPromptSubmit injects the anchored routing block on every user
// prompt and, when the prompt mentions memory/preferences/past work,
// pre-fetches the top-N hits via BM25 and ships them as additionalContext.
// The agent sees relevant memories before deciding whether to call
// anchored_search — making the right answer the path of least resistance.
func runHookUserPromptSubmit(args []string) {
	fs := newFlagSet("hook userpromptsubmit")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	body, _ := io.ReadAll(os.Stdin)

	// Cursor's beforeSubmitPrompt maps here, but Cursor IGNORES this hook's
	// stdout — so there is no recall injection to emit. The Cursor adapter does
	// capture-only work and emits nothing. Claude Code payloads never match, so
	// the injection path below is unchanged for them.
	if hookroute.DetectCursorPayload(body) {
		handleCursorHook(os.Stdout, body, *configPath, dlog)
		return
	}

	var parsed struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
		Prompt    string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &parsed)

	// Compact reminder each turn; the full routing block is injected once per
	// session (SessionStart + MCP initialize), so we don't re-pay ~2KB here.
	additional := mcp.AnchoredRoutingReminder

	// Intent-aware auto-recall runs on EVERY prompt within a tight budget. It
	// puts relevant memories (and, for debugging, recent artifacts) in front of
	// the model rather than hoping it calls anchored_search, and self-gates:
	// trivial/unknown prompts inject nothing. Gated by plugin.auto_recall.
	if preview := autoRecallPreview(*configPath, parsed.Cwd, parsed.Prompt, parsed.SessionID, dlog); preview != "" {
		additional += "\n\n" + preview
	}

	dlog.Event("hook.userpromptsubmit", map[string]any{
		"stage":         "emitted",
		"session_id":    parsed.SessionID,
		"prompt_len":    len(parsed.Prompt),
		"prompt_head":   debuglog.Snippet(parsed.Prompt, 240),
		"context_bytes": len(additional),
	})

	outputJSON(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": additional,
		},
	})
}

// categoriesForIntent maps a detected intent to the memory categories worth
// boosting in retrieval. An empty result means "no category filter" (search
// everything) — used for code_change and unknown where the relevant memory
// could be any kind.
func categoriesForIntent(k intent.Kind) []string {
	switch k {
	case intent.KindPlanning, intent.KindArchitecture:
		return []string{"decision", "plan", "learning"}
	case intent.KindDebugging:
		return []string{"learning", "decision"}
	case intent.KindRelease:
		return []string{"decision", "plan", "summary"}
	case intent.KindSecurity:
		return []string{"decision", "learning"}
	default:
		return nil
	}
}

// autoRecallPreview is the intent-aware retrieval path. It classifies the
// prompt, runs a (optionally category-boosted) BM25 query, and for
// debugging/test intents in "full" mode also surfaces recent artifacts
// (test reports, stack traces). Returns "" on any failure, empty result, or
// when auto_recall is off — it must NEVER prevent the prompt from going through
// and must never exit non-zero.
func autoRecallPreview(configPath, cwd, prompt, sessionID string, dlog *debuglog.Logger) string {
	q := sanitizeFTSQuery(prompt)
	if q == "" {
		return ""
	}

	hc, err := openHookContextReadOnly(configPath)
	if err != nil {
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "ctx_init_failed", "error": err.Error()})
		return ""
	}
	defer hc.Close()

	mode := hc.cfg.Plugin.AutoRecallMode()
	if mode == "off" {
		return ""
	}

	in := intent.Detect(prompt)

	// Unknown intent is the noisy case: only retrieve when the prompt carries
	// enough signal (>= recallMinTokens meaningful tokens), so chit-chat that
	// happens to share a word with a memory doesn't trigger injection.
	if in.Kind == intent.KindUnknown && len(strings.Fields(q)) < recallMinTokens {
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "below_threshold", "tokens": len(strings.Fields(q))})
		return ""
	}

	cwdVal := cwd
	if cwdVal == "" {
		cwdVal = "."
	}
	projectID := hc.ResolveProject(cwdVal)

	// ── G2: extract file/symbol anchors from prompt ──────────────────────────
	fileAnchors, symAnchors := extractAnchors(prompt)

	// ── G2: working set for the session ──────────────────────────────────────
	var wsSignals *memory.WorkingSetSignals
	if sessionID != "" {
		wsMgr := session.NewManager(hc.db, nil)
		wsCtx, wsCancel := context.WithTimeout(context.Background(), preSearchTimeout)
		ws, wsErr := wsMgr.GetWorkingSet(wsCtx, sessionID)
		wsCancel()
		if wsErr == nil && ws != nil && !ws.Empty() {
			wsSignals = &memory.WorkingSetSignals{
				Files:    ws.Files,
				Symbols:  ws.Symbols,
				Entities: ws.Entities,
			}
		}
	}

	// ── G2: build expanded query (anchors have precedence, cap 24) ──────────
	expandedQ := buildExpandedQuery(q, fileAnchors, symAnchors, wsSignals)

	ctx, cancel := context.WithTimeout(context.Background(), preSearchTimeout)
	defer cancel()

	hits, err := bm25TopHits(ctx, hc.db, expandedQ, projectID, preSearchLimit, categoriesForIntent(in.Kind)...)
	if err != nil {
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "query_failed", "error": err.Error()})
		return ""
	}

	// ── G2: local re-rank — boost hits that mention anchor/working-set tokens ─
	allAnchorTokens := anchorTokens(fileAnchors, symAnchors, wsSignals)
	hits = reRankHits(hits, allAnchorTokens, fileAnchors, wsSignals)

	// ── P1-1: memory-to-memory vector expansion (full mode only) ─────────────
	// Keyword recall can miss memories that are semantically related but share
	// no literal tokens. We can't embed the raw prompt in-hook (separate
	// process, no warm ONNX embedder, mutex + cold-start blow the budget), but
	// we CAN use the top BM25 hit's PRECOMPUTED embedding as a seed vector and
	// pull its nearest neighbours from the vector cache — real vector recall
	// with zero embedding work. Gated to "full" mode and to a non-saturated BM25
	// result (room to add value); fully bounded + fail-open so it can never
	// delay or block the prompt.
	if mode == "full" && len(hits) > 0 && len(hits) < preSearchLimit {
		hits = vectorExpandHits(hc.db, hits, projectID, dlog)
	}

	// For debugging/test intents in "full" mode, surface the most recent
	// captured artifacts so the model can pull the failing test output or
	// stack trace without guessing it exists.
	var arts []recentArtifact
	if mode == "full" && (in.Kind == intent.KindDebugging || in.Kind == intent.KindTestExecution) {
		arts = recentArtifacts(ctx, hc.db, projectID, []string{"test_report", "stack_trace", "build_report"}, 3)
	}

	if len(hits) == 0 && len(arts) == 0 {
		// Semantic-miss recovery. We reached here only because the prompt
		// cleared the retrieval threshold (trivial/below-threshold prompts
		// already returned ""), yet keyword (BM25) recall found nothing. That
		// is exactly the case the BM25-only push is blind to: a prompt that is
		// semantically related to a stored memory but shares no literal tokens.
		// True in-hook vector recall is not possible cheaply (the hook is a
		// separate process with no warm ONNX embedder, and embedding a fresh
		// prompt would block on the embedder mutex past the hook budget), so we
		// do the next best, zero-cost thing: tell the model that keyword recall
		// came up empty and to call anchored_search, whose hybrid BM25+vector
		// search in the serve process CAN catch the semantic match. This turns a
		// silent miss into a directed tool call instead of just the generic
		// reminder.
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "no_hits", "intent": string(in.Kind), "query": debuglog.Snippet(expandedQ, 80)})
		return renderRecallMissNudge(expandedQ)
	}

	// ── G2: KG lookup for anchors ─────────────────────────────────────────────
	var kgLine string
	if len(fileAnchors)+len(symAnchors) > 0 {
		kgLine = queryKGForAnchors(hc.db, fileAnchors, symAnchors, projectID)
	}

	dlog.Event("hook.userpromptsubmit.recall", map[string]any{
		"stage":        "hits",
		"intent":       string(in.Kind),
		"hits":         len(hits),
		"artifacts":    len(arts),
		"query":        debuglog.Snippet(expandedQ, 80),
		"project":      projectID,
		"anchor_files": len(fileAnchors),
		"anchor_syms":  len(symAnchors),
	})

	// ── G2: adaptive reminder ────────────────────────────────────────────────
	var adaptiveMode adaptiveReminderMode
	if hc.cfg.Plugin.AdaptiveReminderEnabled() {
		adaptiveMode = classifyAdaptiveReminder(hits)
	}

	// ── D-lite: record which memories were injected (usage-feedback capture).
	// Best-effort with a HARD wall-clock cap on the whole call — config load,
	// EnsureDirs and sql.Open included, not just the DB writes. A plain
	// goroutine would not work here: the hook process exits right after
	// emitting its JSON, killing any in-flight async work — so we run the
	// write concurrently but wait up to the cap, letting the common case
	// complete and a slow filesystem skip tracking instead of delaying the
	// prompt.
	if len(hits) > 0 {
		done := make(chan struct{})
		go func() {
			defer close(done)
			recordInjection(configPath, sessionID, projectID, hits, dlog)
		}()
		select {
		case <-done:
		case <-time.After(injectionTotalBudget):
			dlog.Event("hook.userpromptsubmit.inject_track", map[string]any{"stage": "total_budget_exceeded"})
		}
	}

	preview := renderRecallPreview(expandedQ, in.Kind, hits, arts, kgLine, adaptiveMode, hc.cfg.Plugin.HookBudget())

	// Credit the context gate off the RENDERED preview, never off the hit
	// count: renderRecallPreview drops hits to fit the byte budget and returns
	// "" when even the top hit overflows, so retrieving memories is not the
	// same as delivering them. Crediting on hits alone would open the gate for
	// a session that received nothing — precisely what the gate exists to
	// prevent.
	creditGateFromRecall(hc.cfg, sessionID, preview, dlog)

	// Record token telemetry for `anchored stats --tokens`. Best-effort and
	// fast (local writes); happens before the caller emits so it never delays
	// the injected context beyond a sub-millisecond insert.
	recordRecall(hc.db, projectID, "userpromptsubmit",
		contextbudget.ApproxTokens(preview),
		projectBaselineTokens(hc.db, projectID, cwdVal, time.Now()))

	return preview
}

// injectionWriteTimeout bounds the DB writes of injection tracking.
const injectionWriteTimeout = 50 * time.Millisecond

// injectionTotalBudget is the hard wall-clock cap for the entire tracking
// call, including config load and directory/DB setup that the inner context
// cannot bound.
const injectionTotalBudget = 100 * time.Millisecond

// creditGateFromRecall satisfies the context gate when this recall actually
// delivered memories to the model. It is the UserPromptSubmit half of the
// recall-credit rule; the SessionStart half writes the companion marker that
// satisfyGateFromRecall requires. Reports whether the gate was credited.
//
// Taking the rendered preview (not the hit slice) is deliberate — see the call
// site. A no-op when the gate is disabled or nothing was rendered.
func creditGateFromRecall(cfg *config.Config, sessionID, preview string, dlog *debuglog.Logger) bool {
	if cfg == nil || preview == "" || cfg.Plugin.ContextGateMode() != "enforce" {
		return false
	}
	if !satisfyGateFromRecall(cfg.Memory.StorageDir, sessionID) {
		return false
	}
	dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "context_gate_satisfied"})
	return true
}

// recordInjection captures the usage-feedback signal for Feature D: which
// memories were put in front of the model this turn. It merges the IDs into
// the session working set (working_sets.memory_ids, existing cap/dedup) and
// bumps injected_count/last_injected_at in each memory's metadata so a future
// curation pass can demote always-injected-never-used memories. Fail-safe and
// fire-and-forget: any error is logged at debug level and ignored.
func recordInjection(configPath, sessionID, projectID string, hits []preSearchHit, dlog *debuglog.Logger) {
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.ID != "" {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 {
		return
	}

	whc, err := openHookContextWrite(configPath)
	if err != nil {
		dlog.Event("hook.userpromptsubmit.inject_track", map[string]any{"stage": "open_failed", "error": err.Error()})
		return
	}
	defer whc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), injectionWriteTimeout)
	defer cancel()

	if sessionID != "" {
		mgr := session.NewManager(whc.db, nil)
		if _, err := mgr.UpdateWorkingSet(ctx, sessionID, session.WorkingSetDelta{
			ProjectID: projectID,
			MemoryIDs: ids,
		}); err != nil {
			dlog.Event("hook.userpromptsubmit.inject_track", map[string]any{"stage": "ws_failed", "error": err.Error()})
		}
	}

	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	// The NULLIF chain normalises legacy rows whose metadata column holds ''
	// or the JSON literal 'null' (json_set would fail on a non-object root).
	if _, err := whc.db.ExecContext(ctx, `
		UPDATE memories SET metadata = json_set(
			COALESCE(NULLIF(NULLIF(metadata, ''), 'null'), '{}'),
			'$.injected_count', COALESCE(json_extract(metadata, '$.injected_count'), 0) + 1,
			'$.last_injected_at', ?
		) WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		dlog.Event("hook.userpromptsubmit.inject_track", map[string]any{"stage": "count_failed", "error": err.Error()})
		return
	}
	dlog.Event("hook.userpromptsubmit.inject_track", map[string]any{"stage": "tracked", "ids": len(ids)})
}

// adaptiveReminderMode controls which reminder variant to emit.
type adaptiveReminderMode int

const (
	adaptiveReminderDefault adaptiveReminderMode = iota // standard reminder (unchanged)
	adaptiveReminderStrong                              // >= 1 boosted hit: "memories injected — consult before exploring"
	adaptiveReminderShort                               // 0 hits: short 1-line reminder
)

// classifyAdaptiveReminder decides the reminder mode based on hit signals.
func classifyAdaptiveReminder(hits []preSearchHit) adaptiveReminderMode {
	if len(hits) == 0 {
		return adaptiveReminderShort
	}
	for _, h := range hits {
		for _, sig := range h.Signals {
			if sig == "file_anchor" || sig == "working_set" {
				return adaptiveReminderStrong
			}
		}
	}
	return adaptiveReminderDefault
}

// preSearchHit is a BM25 hit with optional explainability signals.
type preSearchHit struct {
	ID       string
	Category string
	Content  string
	Signals  []string // e.g. "file_anchor", "working_set"
}

// extractAnchors extracts file-path tokens and symbol tokens from a prompt.
//
// File anchor: token contains '/' and ends in .<ext> (1-5 alphanum chars),
// OR token has no '/' but ends in .<ext> (e.g. "foo.go").
// Symbol: identifier that is CamelCase or snake_case with >= 4 runes and
// does not look like a plain word (has uppercase after start OR contains '_').
func extractAnchors(prompt string) (fileAnchors []string, symAnchors []string) {
	seen := make(map[string]bool)
	for _, tok := range strings.Fields(prompt) {
		// Strip surrounding punctuation but keep internal . / _
		tok = strings.Trim(tok, "\"'`()[]{},:;!?")
		if tok == "" {
			continue
		}
		if isFileAnchor(tok) {
			base := fileBasename(tok)
			if !seen[base] {
				seen[base] = true
				fileAnchors = append(fileAnchors, tok)
			}
			continue
		}
		if isSymbolAnchor(tok) {
			low := strings.ToLower(tok)
			if !seen["sym:"+low] {
				seen["sym:"+low] = true
				symAnchors = append(symAnchors, tok)
			}
		}
	}
	return
}

// isFileAnchor returns true if tok looks like a file path reference.
func isFileAnchor(tok string) bool {
	hasSlash := strings.ContainsAny(tok, "/\\")
	dotIdx := strings.LastIndex(tok, ".")
	if dotIdx < 0 {
		return false
	}
	ext := tok[dotIdx+1:]
	if len(ext) < 1 || len(ext) > 5 {
		return false
	}
	for _, r := range ext {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	if hasSlash {
		return true
	}
	// No slash: only a file if there's at least one non-dot char before the ext
	return dotIdx > 0
}

// isSymbolAnchor returns true if tok looks like a CamelCase or snake_case identifier.
func isSymbolAnchor(tok string) bool {
	runes := []rune(tok)
	if len(runes) < 4 {
		return false
	}
	// Must be purely letters/digits/underscore
	for _, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	// CamelCase: has uppercase after the first rune
	for _, r := range runes[1:] {
		if unicode.IsUpper(r) {
			return true
		}
	}
	// snake_case: contains underscore
	return strings.Contains(tok, "_")
}

// fileBasename returns the basename of a file path token.
func fileBasename(tok string) string {
	base := filepath.Base(tok)
	return strings.ToLower(base)
}

// anchorTokens flattens fileAnchors, symAnchors, and wsSignals into the
// deduped lower-cased set of tokens used for re-ranking.
func anchorTokens(fileAnchors, symAnchors []string, ws *memory.WorkingSetSignals) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len([]rune(s)) < 3 || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, f := range fileAnchors {
		base := fileBasename(f)
		add(base)
		if dot := strings.LastIndex(base, "."); dot > 0 {
			if stem := base[:dot]; len([]rune(stem)) >= 4 {
				add(stem)
			}
		}
	}
	for _, s := range symAnchors {
		add(s)
	}
	if ws != nil {
		for _, f := range ws.Files {
			base := fileBasename(f)
			add(base)
			if dot := strings.LastIndex(base, "."); dot > 0 {
				if stem := base[:dot]; len([]rune(stem)) >= 4 {
					add(stem)
				}
			}
		}
		for _, s := range ws.Symbols {
			add(s)
		}
		for _, e := range ws.Entities {
			add(e)
		}
	}
	return out
}

// buildExpandedQuery builds a BM25 query string from free-text tokens plus
// anchor tokens. Anchor tokens have precedence; total capped at anchorQueryCap.
func buildExpandedQuery(freeText string, fileAnchors, symAnchors []string, ws *memory.WorkingSetSignals) string {
	// Anchor tokens first (deduplicated, sanitized for FTS).
	anchorToks := anchorTokens(fileAnchors, symAnchors, ws)
	var sanitizedAnchors []string
	for _, tok := range anchorToks {
		san := sanitizeFTSQuery(tok)
		if san != "" {
			sanitizedAnchors = append(sanitizedAnchors, strings.Fields(san)...)
		}
	}

	// Free-text tokens (already sanitized).
	freeToks := strings.Fields(freeText)

	// Merge: anchors first, then fill with free-text up to cap. The cap also
	// bounds the anchor loop itself — a 50-file working set would otherwise
	// produce an uncapped OR query that degrades BM25 (everything matches
	// weakly) and risks the FTS5 parser limits.
	seen := make(map[string]bool)
	var combined []string
	for _, t := range sanitizedAnchors {
		if len(combined) >= anchorQueryCap {
			break
		}
		if !seen[t] {
			seen[t] = true
			combined = append(combined, t)
		}
	}
	for _, t := range freeToks {
		if len(combined) >= anchorQueryCap {
			break
		}
		if !seen[t] {
			seen[t] = true
			combined = append(combined, t)
		}
	}
	if len(combined) == 0 {
		return freeText
	}
	return strings.Join(combined, " ")
}

// reRankHits applies a local 1.3x boost to hits that mention anchor/ws tokens
// and annotates signals. The input slice is modified in-place and returned
// sorted by descending pseudo-score (boosted hits first).
func reRankHits(hits []preSearchHit, allTokens []string, fileAnchors []string, ws *memory.WorkingSetSignals) []preSearchHit {
	if len(allTokens) == 0 {
		return hits
	}

	// Build separate sets for file-anchor tokens and ws tokens for signal labelling.
	fileSet := make(map[string]bool)
	for _, f := range fileAnchors {
		base := strings.ToLower(fileBasename(f))
		fileSet[base] = true
		if dot := strings.LastIndex(base, "."); dot > 0 {
			if stem := base[:dot]; len([]rune(stem)) >= 4 {
				fileSet[stem] = true
			}
		}
	}

	wsTokenSet := make(map[string]bool)
	if ws != nil {
		for _, tok := range anchorTokens(nil, nil, ws) {
			wsTokenSet[tok] = true
		}
	}

	scoredHits := make([]scoredHit, len(hits))
	for i, h := range hits {
		hay := strings.ToLower(h.Content)
		s := scoredHit{hit: h, score: float64(len(hits) - i), order: i} // base score: BM25 rank
		boosted := false
		for _, tok := range allTokens {
			if strings.Contains(hay, tok) {
				if !boosted {
					s.score *= 1.3
					boosted = true
				}
				if fileSet[tok] {
					s.hit.Signals = appendUnique(s.hit.Signals, "file_anchor")
				} else if wsTokenSet[tok] {
					s.hit.Signals = appendUnique(s.hit.Signals, "working_set")
				}
			}
		}
		scoredHits[i] = s
	}

	// Stable sort: higher score first; ties preserve BM25 order.
	stableSort(scoredHits)

	out := make([]preSearchHit, len(scoredHits))
	for i, s := range scoredHits {
		out[i] = s.hit
	}
	return out
}

// scoredHit wraps a preSearchHit with a floating-point score and original
// position for stable sort during re-ranking.
type scoredHit struct {
	hit   preSearchHit
	score float64
	order int
}

// stableSort sorts a slice of scored hits by score descending, preserving
// original order for ties (insertion sort is fine for N<=5).
func stableSort(items []scoredHit) {
	n := len(items)
	for i := 1; i < n; i++ {
		key := items[i]
		j := i - 1
		for j >= 0 && (items[j].score < key.score || (items[j].score == key.score && items[j].order > key.order)) {
			items[j+1] = items[j]
			j--
		}
		items[j+1] = key
	}
}

func appendUnique(sigs []string, s string) []string {
	for _, existing := range sigs {
		if existing == s {
			return sigs
		}
	}
	return append(sigs, s)
}

// queryKGForAnchors queries the KG for up to 2 triples matching anchor tokens.
// Returns a compact `<anchored_kg>` line or "" when nothing found or on error.
func queryKGForAnchors(db *sql.DB, fileAnchors, symAnchors []string, projectID string) string {
	kgInst := kg.New(db, nil)

	candidates := make([]string, 0, len(fileAnchors)+len(symAnchors))
	for _, f := range fileAnchors {
		base := fileBasename(f)
		if dot := strings.LastIndex(base, "."); dot > 0 {
			candidates = append(candidates, base[:dot])
		}
		candidates = append(candidates, base)
	}
	for _, s := range symAnchors {
		candidates = append(candidates, s)
	}

	var allTriples []string
	seen := make(map[string]bool)
	for _, name := range candidates {
		if len(allTriples) >= 2 {
			break
		}
		kgCtx, kgCancel := context.WithTimeout(context.Background(), kgQueryTimeout)
		var pid *string
		if projectID != "" {
			p := projectID
			pid = &p
		}
		triples, err := kgInst.Query(kgCtx, name, pid)
		kgCancel()
		if err != nil || len(triples) == 0 {
			continue
		}
		for _, tr := range triples {
			if len(allTriples) >= 2 {
				break
			}
			line := tr.Subject + " " + tr.Predicate + " " + tr.Object
			if !seen[line] {
				seen[line] = true
				allTriples = append(allTriples, escapeText(line))
			}
		}
	}
	if len(allTriples) == 0 {
		return ""
	}
	return "<anchored_kg>" + strings.Join(allTriples, "; ") + "</anchored_kg>"
}

// bm25TopHits runs a project-scoped (with global fallback) BM25 query and
// returns up to `limit` hits. When categories are given, results are filtered
// to those memory categories (the intent-aware boost); empty means no filter.
func bm25TopHits(ctx context.Context, db *sql.DB, q string, projectID string, limit int, categories ...string) ([]preSearchHit, error) {
	// FTS5's default operator between bare terms is AND, which makes a
	// free-form prompt match only memories containing every word — almost
	// never what we want for recall. OR the tokens so any overlapping term
	// contributes, and let bm25() rank by how many/how strongly they match.
	match := strings.Join(strings.Fields(q), " OR ")
	if match == "" {
		return nil, nil
	}

	// low_signal exclusion closes the usage-feedback loop on the push path:
	// without it, a memory demoted by curation (quality or never_used) would
	// keep being injected by this hook even though Service.Search demotes it.
	sqlStmt := `
		SELECT m.id, m.category, m.content
		FROM memories_fts fts
		JOIN memories m ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ?
		  AND m.deleted_at IS NULL
		  AND COALESCE(json_extract(m.metadata, '$.curation_status'), '') != 'low_signal'
		  AND (? = '' OR m.project_id = ?)`
	args := []any{match, projectID, projectID}
	if len(categories) > 0 {
		placeholders := make([]string, len(categories))
		for i, c := range categories {
			placeholders[i] = "?"
			args = append(args, c)
		}
		sqlStmt += " AND m.category IN (" + strings.Join(placeholders, ",") + ")"
	}
	sqlStmt += " ORDER BY bm25(memories_fts) ASC LIMIT ?"
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlStmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []preSearchHit
	for rows.Next() {
		var h preSearchHit
		if err := rows.Scan(&h.ID, &h.Category, &h.Content); err != nil {
			continue
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// recentArtifact is a lightweight view of a captured artifact for the recall
// preview — enough for the model to know it exists and search it.
type recentArtifact struct {
	Type        string
	SourceLabel string
	AgeHint     string
}

// recentArtifacts returns the newest artifacts of the given types for the
// project (empty projectID matches all). Best-effort: returns nil on any error
// (e.g. an older DB without the artifacts table). Never blocks the hook.
func recentArtifacts(ctx context.Context, db *sql.DB, projectID string, types []string, limit int) []recentArtifact {
	if len(types) == 0 {
		return nil
	}
	placeholders := make([]string, len(types))
	args := []any{projectID, projectID}
	for i, t := range types {
		placeholders[i] = "?"
		args = append(args, t)
	}
	args = append(args, limit)
	stmt := `
		SELECT type, source_label, created_at
		FROM artifacts
		WHERE (? = '' OR project_id = ?)
		  AND type IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY created_at DESC
		LIMIT ?`

	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var out []recentArtifact
	for rows.Next() {
		var a recentArtifact
		var created string
		if err := rows.Scan(&a.Type, &a.SourceLabel, &created); err != nil {
			continue
		}
		a.AgeHint = created
		out = append(out, a)
	}
	return out
}

// sanitizeFTSQuery strips FTS5 syntax so a free-form prompt becomes a safe
// MATCH expression. We keep alphanumerics and accented letters, replace
// everything else with spaces, lowercase, and collapse runs of whitespace.
// FTS5 with default tokenizer treats this as a bag-of-tokens OR query —
// what we want for prompt-driven retrieval. Resulting query is capped at
// 16 tokens to keep BM25 ranking focused.
func sanitizeFTSQuery(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	tokens := strings.Fields(b.String())
	// Drop very short tokens (1-2 chars) — they're stop-noise and hurt BM25.
	filtered := tokens[:0]
	for _, t := range tokens {
		if len([]rune(t)) >= 3 {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) > 16 {
		filtered = filtered[:16]
	}
	return strings.Join(filtered, " ")
}

// Vector-expansion tuning. These bound the cost so the push stays inside the
// hook budget and never embeds the raw prompt.
const (
	// vectorExpandBudget hard-caps the whole expansion (cache load + score +
	// fetch). On timeout we return the BM25 hits unchanged — fail-open.
	vectorExpandBudget = 100 * time.Millisecond
	// vectorExpandTopK is how many neighbours Score returns before filtering.
	vectorExpandTopK = 8
	// vectorExpandMax caps how many NEW memories we merge into the push.
	vectorExpandMax = 3
	// vectorExpandMinScore is the cosine floor for a neighbour to be worth
	// surfacing — below this the "semantic" link is noise.
	vectorExpandMinScore = 0.6
)

// vectorExpandHits augments the BM25 hits with semantically-near memories using
// the top hit's precomputed embedding as the seed (no raw-prompt embedding).
// The vector cache load is unbounded by ctx, so the whole computation runs in a
// goroutine guarded by vectorExpandBudget; on timeout/empty/error the original
// hits are returned unchanged. Fail-open by contract.
func vectorExpandHits(db *sql.DB, hits []preSearchHit, projectID string, dlog *debuglog.Logger) []preSearchHit {
	ch := make(chan []preSearchHit, 1) // buffered: a late goroutine never leaks/blocks
	go func() {
		// Discard logger: VectorCache.Load logs an Info line per call, which
		// would spam the hook's stderr on every prompt.
		vc := memory.NewVectorCache(slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := vc.Load(db); err != nil || vc.Len() == 0 {
			ch <- nil
			return
		}
		ch <- expandFromCache(vc, db, hits, projectID)
	}()

	select {
	case extra := <-ch:
		if len(extra) == 0 {
			return hits
		}
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "vector_expanded", "added": len(extra)})
		return append(hits, extra...)
	case <-time.After(vectorExpandBudget):
		dlog.Event("hook.userpromptsubmit.recall", map[string]any{"stage": "vector_expand_timeout"})
		return hits
	}
}

// expandFromCache scores the cache against the top hit's precomputed vector and
// returns up to vectorExpandMax NEW memories (not already in hits) tagged with a
// "vector" signal. Split out from vectorExpandHits so tests can drive it with a
// Put-populated cache instead of a Load from disk.
func expandFromCache(vc *memory.VectorCache, db *sql.DB, hits []preSearchHit, projectID string) []preSearchHit {
	if len(hits) == 0 {
		return nil
	}
	seed, ok := vc.Get(hits[0].ID)
	if !ok || len(seed) == 0 {
		return nil
	}
	norm := memory.VectorNorm(seed)
	if norm == 0 {
		return nil
	}

	existing := make(map[string]bool, len(hits))
	for _, h := range hits {
		existing[h.ID] = true
	}

	var newIDs []string
	for _, s := range vc.Score(seed, norm, vectorExpandMinScore, vectorExpandTopK) {
		if existing[s.ID] {
			continue // already in the BM25 set (incl. the seed itself, score 1.0)
		}
		newIDs = append(newIDs, s.ID)
		if len(newIDs) >= vectorExpandMax {
			break
		}
	}
	if len(newIDs) == 0 {
		return nil
	}

	out := fetchMemoriesByIDs(db, newIDs, projectID)
	for i := range out {
		out[i].Signals = appendUnique(out[i].Signals, "vector")
	}
	return out
}

// fetchMemoriesByIDs loads id/category/content for the given memory IDs, keeping
// the same liveness/quality filters as bm25TopHits (skip deleted + low_signal,
// project-scope when projectID != ""). Order follows the input ids slice.
func fetchMemoriesByIDs(db *sql.DB, ids []string, projectID string) []preSearchHit {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, projectID, projectID)
	stmt := `SELECT id, category, content FROM memories
		WHERE id IN (` + strings.Join(placeholders, ",") + `)
		  AND deleted_at IS NULL
		  AND COALESCE(json_extract(metadata, '$.curation_status'), '') != 'low_signal'
		  AND (? = '' OR project_id = ?)`

	ctx, cancel := context.WithTimeout(context.Background(), preSearchTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	byID := make(map[string]preSearchHit, len(ids))
	for rows.Next() {
		var h preSearchHit
		if err := rows.Scan(&h.ID, &h.Category, &h.Content); err != nil {
			continue
		}
		byID[h.ID] = h
	}
	// Preserve the (relevance-ordered) input order.
	out := make([]preSearchHit, 0, len(ids))
	for _, id := range ids {
		if h, ok := byID[id]; ok {
			out = append(out, h)
		}
	}
	return out
}

// renderRecallMissNudge builds the semantic-miss nudge injected when keyword
// recall found nothing for a threshold-passing prompt. It is intentionally
// short and adds no DB/embedding work: its whole job is to redirect the model
// to anchored_search (hybrid BM25+vector in the serve process), which can find
// the semantic match that the hook's keyword-only pass cannot. The query is
// echoed (rune-capped, escaped) so the model sees what was already tried.
func renderRecallMissNudge(query string) string {
	return fmt.Sprintf("<anchored_recall query=%q hits=\"0\">\n"+
		"  Keyword recall found no stored memories for this prompt. If it touches prior work, "+
		"decisions, conventions, preferences, or a named project/library, call anchored_search "+
		"before relying on the codebase — its hybrid semantic+keyword search catches matches this "+
		"keyword-only pass misses.\n</anchored_recall>", truncateRunes(query, 80))
}

// renderRecallPreview renders the recall block under a byte budget. Hits are
// already relevance-ordered (best/boosted first after re-rank); when the block
// would exceed the budget we drop the lowest-relevance trailing hits rather
// than truncating a hit mid-content, so what survives is always coherent and
// the most relevant.
func renderRecallPreview(query string, kind intent.Kind, hits []preSearchHit, arts []recentArtifact, kgLine string, adaptive adaptiveReminderMode, budget int) string {
	render := func(hh []preSearchHit) string {
		var sb strings.Builder
		sigStr := signalsAttr(hh)
		if sigStr != "" {
			fmt.Fprintf(&sb, "<anchored_recall intent=%q query=%q count=%q signals=%q>\n",
				escapeText(string(kind)), truncateRunes(query, 80), fmt.Sprintf("%d", len(hh)), sigStr)
		} else {
			fmt.Fprintf(&sb, "<anchored_recall intent=%q query=%q count=%q>\n",
				escapeText(string(kind)), truncateRunes(query, 80), fmt.Sprintf("%d", len(hh)))
		}
		for _, h := range hh {
			content := strings.ReplaceAll(h.Content, "\n", " ")
			content = strings.ReplaceAll(content, "\r", " ")
			content = truncateRunes(content, 240)
			if len(h.Signals) > 0 {
				fmt.Fprintf(&sb, "  [%s|%s] %s\n", escapeText(h.Category), strings.Join(h.Signals, ","), escapeText(content))
			} else {
				fmt.Fprintf(&sb, "  [%s] %s\n", escapeText(h.Category), escapeText(content))
			}
		}
		for _, a := range arts {
			fmt.Fprintf(&sb, "  <artifact type=%q label=%q/> (search with: anchored artifact search)\n",
				escapeText(a.Type), escapeText(truncateRunes(a.SourceLabel, 80)))
		}
		if kgLine != "" {
			fmt.Fprintf(&sb, "  %s\n", kgLine)
		}
		sb.WriteString("</anchored_recall>")

		// Append adaptive reminder after the closing tag.
		switch adaptive {
		case adaptiveReminderStrong:
			sb.WriteString("\n<!-- anchored: memórias relevantes injetadas acima — consulte antes de explorar arquivos -->")
		case adaptiveReminderShort:
			sb.WriteString("\n<!-- anchored: use anchored_search para memórias adicionais -->")
		}

		return sb.String()
	}

	// Drop trailing (lowest-relevance) hits until it fits. Artifacts are few
	// and short; keep them. Always emit at least the top hit if present.
	for n := len(hits); n >= 0; n-- {
		out := render(hits[:n])
		if len(out) <= budget || n == 0 {
			if n == 0 && len(arts) == 0 {
				return ""
			}
			return out
		}
	}
	return ""
}

// signalsAttr returns a comma-joined summary of all unique signals in a hit
// list, for the outer anchored_recall attribute. Empty when no signals.
func signalsAttr(hits []preSearchHit) string {
	seen := make(map[string]bool)
	var out []string
	for _, h := range hits {
		for _, s := range h.Signals {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return strings.Join(out, ",")
}

// truncateRunes caps `s` at `max` runes (NOT bytes) so we never split a
// multibyte UTF-8 sequence. Mirrors the helper in pkg/mcp; intentionally
// duplicated to keep cmd/anchored free of internal pkg/mcp imports beyond
// the routing block constant.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "…"
}

// escapeText escapes the three character-data hostiles for XML; quotes are
// left as-is so prose remains readable inside <anchored_search_preview>.
func escapeText(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}
