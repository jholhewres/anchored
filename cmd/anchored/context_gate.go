package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/hookroute"
)

// The context gate is the only DETERMINISTIC channel anchored has to make the
// agent actually consult its persistent memory. Soft injection (the
// SessionStart routing block, the UserPromptSubmit recall) can be — and often
// is — ignored when the user hands the model a concrete, actionable task: the
// model jumps straight to git/grep/edit and never reads what memory already
// knows. The gate closes that hole by DENYING the first substantive tool call
// of a session until the agent has called anchored_context (or a search), which
// surfaces the deny reason to the model and makes consulting memory the only
// way forward.
//
// It is deliberately bounded so it can never become a productivity tax:
//   - fires AT MOST once per session (the marker is sticky once satisfied),
//   - relents after ctxGateMaxDenies so a model that insists on ignoring the
//     redirect is eventually let through rather than wedged,
//   - is fully fail-open: a missing session id or an unwritable state dir means
//     passthrough, never a block.
//
// State lives in one tiny marker file per session under <storage>/ctxgate/.
// We intentionally avoid the DB on this hot path: PreToolUse runs on every
// single tool call, so an os.Stat/ReadFile is the right cost, not a sql.Open.

// ctxGateMaxDenies is how many times the gate will deny work tools before
// relenting for the rest of the session. A clear deny reason usually lands on
// the first try; the budget exists purely so a non-compliant model can never be
// permanently blocked from working.
const ctxGateMaxDenies = 3

// ctxGateMarkerTTL bounds how long stale per-session markers linger before the
// lazy sweep on the (rare) deny path removes them.
const ctxGateMarkerTTL = 7 * 24 * time.Hour

// ctxGateSatisfied is the content written into the satisfaction sentinel, and
// the legacy content that older builds wrote into the counter file itself.
const ctxGateSatisfied = "ok"

// ctxGateOKMarkerSuffix names the satisfaction sentinel, a file distinct from
// the counter file. Satisfaction USED to be a value inside the counter file,
// which made it destructible: a deny write that raced with a credit could
// overwrite "ok" with a stale counter and re-arm a gate the agent had already
// satisfied. A separate file makes the state machine monotonic — once credited,
// no counter write can revoke it, whatever the interleaving.
const ctxGateOKMarkerSuffix = ".ok"

// contextGateSatisfyingTools are the anchored MCP leaf tools whose use proves
// the agent engaged its memory and therefore satisfies the gate for the rest of
// the session. Loading context OR searching both count.
var contextGateSatisfyingTools = map[string]bool{
	"anchored_context":    true,
	"anchored_search":     true,
	"anchored_ctx_search": true,
	"anchored_kg_query":   true,
}

// mcpLeafName strips an MCP server prefix from a wire tool name, returning the
// leaf: mcp__anchored__anchored_save and
// mcp__plugin_anchored_anchored__anchored_save both yield anchored_save. Every
// decision anchored makes about "is this one of my tools" must go through here
// rather than test the wire name, because the registration prefix is chosen by
// the harness and changes without notice. Non-MCP names pass through unchanged.
func mcpLeafName(tool string) string {
	if i := strings.LastIndex(tool, "__"); i >= 0 {
		return tool[i+2:]
	}
	return tool
}

// isAnchoredTool reports whether a tool belongs to anchored itself. Such tools
// must never be gated: gating them would deadlock the session, since the only
// way to satisfy the gate is to call one of them.
//
// It takes the LEAF name only, deliberately. Registration prefixes drift across
// harnesses (mcp__anchored__*, mcp__plugin_anchored_anchored__*, …), so a test
// against the wire name silently stops matching the day a harness renames the
// server — the failure mode that kept the PostToolUse matcher dead for this
// tool's entire history. The caller strips the prefix once; there is no second
// derivation here to fall out of sync with it.
func isAnchoredTool(bareTool string) bool {
	return strings.HasPrefix(bareTool, "anchored_")
}

// contextGateDecision applies the optional PreToolUse context gate. It returns
// a non-nil Decision when the caller must DENY the tool, plus a short stage
// string for debug logging. A nil Decision means passthrough (caller continues
// to routing/allow). It is fail-open by contract: any unexpected condition
// yields passthrough so the gate can never block on infrastructure faults.
//
// storageDir is cfg.Memory.StorageDir (already home-expanded); sessionID is the
// client session id; bareTool is the leaf tool name, stripped by mcpLeafName.
func contextGateDecision(storageDir, sessionID, bareTool string) (*hookroute.Decision, string) {
	if sessionID == "" || storageDir == "" {
		return nil, "skip_no_session"
	}

	gateDir := filepath.Join(storageDir, "ctxgate")
	marker := filepath.Join(gateDir, sanitizeSessionID(sessionID))

	// Consulting memory satisfies the gate for the rest of the session. Do this
	// check first so an agent that correctly leads with anchored_context sees
	// zero friction.
	if contextGateSatisfyingTools[bareTool] {
		markGateSatisfied(gateDir, marker)
		return nil, "satisfied"
	}

	// Never gate anchored's own tools (would deadlock).
	if isAnchoredTool(bareTool) {
		return nil, "exempt_anchored"
	}

	// Already satisfied (or relented): never block again this session.
	if gateIsSatisfied(marker) {
		return nil, "already_satisfied"
	}

	denies := readDenyCount(marker)

	// Relent: a model that ignored the redirect ctxGateMaxDenies times is let
	// through so it can never be permanently wedged.
	if denies >= ctxGateMaxDenies {
		markGateSatisfied(gateDir, marker)
		return nil, "relented"
	}

	// Record the deny and block. If we can't even create the state dir, fail
	// open rather than block the user on a filesystem fault.
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		return nil, "skip_mkdir_failed"
	}
	writeGateMarker(gateDir, marker, strconv.Itoa(denies+1))
	sweepStaleGateMarkers(gateDir)

	return &hookroute.Decision{
		Action: hookroute.ActionDeny,
		Reason: "anchored: consult your persistent memory before working. Call " +
			"anchored_context(cwd: \"<this project's absolute path>\") to load identity, " +
			"the project, and recent decisions — your prior work and the user's conventions " +
			"live there, not in the codebase. (Already know what you need? anchored_search works too.) " +
			// The exact MCP registration prefix varies by harness (mcp__anchored__*,
			// mcp__plugin_anchored_anchored__*, …), so the bootstrap hint must search
			// by leaf name — a select: with a hardcoded FQN loads nothing and the
			// model retries the blocked tool, burning deny budget.
			"If the anchored_* tools are not loaded yet (deferred — a direct call fails as not-found), " +
			"FIRST run ToolSearch(query: \"+anchored_ context search\", max_results: 12) and load whatever it " +
			"returns (never assume an exact tool name — the prefix varies by harness), " +
			"THEN call anchored_context — do not retry the blocked tool until you have. " +
			"Calling anchored_context (or a search) clears this gate for the rest of the session. " +
			"It is NOT a permanent block: it auto-relents after a few denies, so consulting memory is the " +
			"way through, not retrying the same tool.",
	}, "denied"
}

// satisfyGateFromPostToolUse is the redundant satisfaction path. PostToolUse
// fires AFTER a tool runs, so if the agent DID call a satisfying anchored tool
// (anchored_context/search/ctx_search/kg_query), mark the gate satisfied for
// the rest of the session — even when the PreToolUse credit was missed. The
// missed-credit case is real: a stale plugin hooks.json whose PreToolUse
// matcher doesn't fire for mcp__ tools would otherwise let the gate deny the
// agent's work three times and relent, never crediting the memory call. This
// PostToolUse path closes that hole. Best-effort and a no-op unless the tool is
// a satisfying anchored tool. Returns true when it wrote the satisfied marker.
func satisfyGateFromPostToolUse(storageDir, sessionID, bareTool string) bool {
	if storageDir == "" || sessionID == "" || !contextGateSatisfyingTools[bareTool] {
		return false
	}
	gateDir := filepath.Join(storageDir, "ctxgate")
	markGateSatisfied(gateDir, filepath.Join(gateDir, sanitizeSessionID(sessionID)))
	return true
}

// ─── Recall-side satisfaction ───────────────────────────────────────────────
//
// The gate exists to guarantee ONE thing: that memory reached the model before
// it started working. A tool call is not the only way that happens — the
// UserPromptSubmit auto-recall retrieves and injects memories directly into the
// context, and SessionStart injects the rich block (identity + project + recent
// decisions). When both land, the gate's precondition is already met and
// denying the first work tool buys nothing: measured over 95 gated sessions, 53%
// were denied with memories already injected.
//
// Crediting is deliberately conservative — hits alone are NOT enough. The
// recall pass is keyword-only and never carries identity (L0), so a session
// that got one weak hit and no rich block has genuinely not seen what
// anchored_context would have shown it. Both signals are required.
//
// The two signals are produced by different hook processes, so SessionStart
// leaves a companion marker next to the gate marker for the recall pass to read.

// ctxGateRichMarkerSuffix names the companion marker written by SessionStart
// when its rich context block was non-empty. Only its EXISTENCE is read; the
// content is a fixed human-readable breadcrumb for anyone inspecting the
// directory, never parsed.
const ctxGateRichMarkerSuffix = ".rich"

// ctxGateRichPresent is that breadcrumb.
const ctxGateRichPresent = "rich-block-emitted"

// richMarkerPath is the companion-marker path for a session.
func richMarkerPath(gateDir, sessionID string) string {
	return filepath.Join(gateDir, sanitizeSessionID(sessionID)+ctxGateRichMarkerSuffix)
}

// markSessionStartRichBlock records that SessionStart emitted a non-empty rich
// context block for this session. Best-effort: a failed write only costs the
// session the recall-side credit, never a block. Swept by the same TTL sweep as
// the gate markers, since it lives in the same directory.
func markSessionStartRichBlock(storageDir, sessionID string) {
	if storageDir == "" || sessionID == "" {
		return
	}
	gateDir := filepath.Join(storageDir, "ctxgate")
	writeGateMarker(gateDir, richMarkerPath(gateDir, sessionID), ctxGateRichPresent)
	// The deny path used to be the only sweep trigger. Now that most sessions
	// are credited without ever denying — and each one writes two markers
	// instead of one — sweeping here is what keeps ctxgate/ bounded. Once per
	// session, off the hot tool-call path.
	sweepStaleGateMarkers(gateDir)
}

// satisfyGateFromRecall credits the gate when SessionStart's rich block landed
// for this session. Whether the recall actually delivered anything is the
// caller's half of the rule: it holds the rendered preview, which is the only
// honest measure of what reached the model, and a hit count here would be a
// second, weaker version of that same check.
//
// Returns true only when THIS call wrote the credit. It runs on every user
// prompt inside a 100ms budget, so an already-credited session must cost one
// stat rather than a fresh MkdirAll + temp file + rename.
func satisfyGateFromRecall(storageDir, sessionID string) bool {
	if storageDir == "" || sessionID == "" {
		return false
	}
	gateDir := filepath.Join(storageDir, "ctxgate")
	marker := filepath.Join(gateDir, sanitizeSessionID(sessionID))
	if gateIsSatisfied(marker) {
		return false
	}
	if _, err := os.Stat(richMarkerPath(gateDir, sessionID)); err != nil {
		return false
	}
	markGateSatisfied(gateDir, marker)
	return true
}

// gateIsSatisfied reports whether this session has already been credited.
func gateIsSatisfied(marker string) bool {
	if _, err := os.Stat(marker + ctxGateOKMarkerSuffix); err == nil {
		return true
	}
	// Legacy state: builds before the sentinel wrote "ok" INTO the counter
	// file. Honor it so upgrading mid-session does not re-arm a gate the agent
	// already satisfied.
	data, err := os.ReadFile(marker)
	return err == nil && strings.TrimSpace(string(data)) == ctxGateSatisfied
}

// markGateSatisfied credits the session. Writing a dedicated sentinel rather
// than a value inside the counter file is what makes the credit permanent: a
// concurrent deny can still bump the counter, but it can no longer erase this.
func markGateSatisfied(gateDir, marker string) {
	writeGateMarker(gateDir, marker+ctxGateOKMarkerSuffix, ctxGateSatisfied)
}

// readDenyCount reads the deny counter, treating anything unparseable (missing
// file, legacy "ok", a torn write) as zero. Zero is the safe reading: it costs
// at most a bounded number of extra denies and can never wedge a session.
func readDenyCount(marker string) int {
	data, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

// writeGateMarker best-effort writes content to a marker, creating the gate dir
// if needed. The write is ATOMIC (temp file + rename): a plain os.WriteFile
// opens O_TRUNC, so a concurrent hook process — and Claude Code issues tool
// calls in parallel batches — can read a zero-length file mid-write and see a
// deny counter of 0, making the relent bound unenforceable. Rename gives every
// reader either the old content or the new, never nothing.
//
// Errors are swallowed: a failed write degrades to "gate not yet satisfied",
// which at worst costs one extra (bounded) deny — never a block.
//
// This does not make the counter's read-modify-write atomic; two concurrent
// denies can still collapse into one increment. That is deliberate — the
// counter only decides WHEN to relent, so losing an increment costs at most an
// extra deny, while the state it must never lose (satisfaction) now lives in
// its own file and is never rewritten by this path.
func writeGateMarker(gateDir, marker, content string) {
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(gateDir, ".tmp-marker-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, marker); err != nil {
		_ = os.Remove(tmpName)
	}
}

// sanitizeSessionID turns a client session id into a filesystem-safe marker
// name. Non [A-Za-z0-9_-] runes become '_'; the result is capped and suffixed.
func sanitizeSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 128 {
		s = s[:128]
	}
	if s == "" {
		s = "_"
	}
	return s + ".gate"
}

// sweepStaleGateMarkers removes markers older than ctxGateMarkerTTL. Best-effort
// and only invoked on the rare deny path, so it never touches the hot allow
// path. Keeps the ctxgate dir from accumulating one file per session forever.
func sweepStaleGateMarkers(gateDir string) {
	entries, err := os.ReadDir(gateDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ctxGateMarkerTTL)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(gateDir, e.Name()))
		}
	}
}
