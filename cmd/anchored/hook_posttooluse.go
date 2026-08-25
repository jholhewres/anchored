package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
	ctxpkg "github.com/jholhewres/anchored/pkg/context"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/hookroute"
	"github.com/jholhewres/anchored/pkg/session"
)

// postToolUseInsertSQL is exposed as a package-level constant so tests can
// run the same SQL against an in-memory database without duplicating it.
// 9 columns / 9 values / 6 placeholders / 6 binds — order matters; do not
// reorder columns without updating both the binds in runHookPostToolUse and
// the schema in pkg/context/migration.go.
const postToolUseInsertSQL = `INSERT INTO session_events
	(id, session_id, project_id, event_type, priority, tool_name, summary, metadata, created_at)
	VALUES (?, ?, ?, 'tool_call', 3, ?, ?, ?, datetime('now'))`

// PostToolUseDeps wires the IO surface and runtime collaborators of the
// posttooluse hook so the core can be exercised without touching os.Stdin/
// os.Stdout or instantiating a HookContext. Production builds wire stdin to
// os.Stdin, stdout to os.Stdout, and DB+ResolveProject through HookContext;
// tests pass an in-memory DB and a stub resolver.
type PostToolUseDeps struct {
	Stdin          io.Reader
	Stdout         io.Writer
	DB             ExecContexter
	ResolveProject func(cwd string) string
	SessionIDFlag  string
	CwdFlag        string
	Now            func() time.Time
	NewID          func() string
	Logger         *debuglog.Logger
	// CaptureArtifact indexes a large tool output as a searchable artifact and
	// returns its id. nil disables artifact capture (the event-only path).
	// Production wires it to the artifact store; tests may stub or omit it.
	CaptureArtifact func(projectID, sessionID, artifactType, sourceTool, sourceLabel, content string) (string, error)
	// UpdateWorkingSet records the files/commands/tests this tool call touched
	// into the session working set so retrieval can boost in-focus memories.
	// nil disables the update. Best-effort; errors never block the tool call.
	UpdateWorkingSet func(sessionID, projectID string, files, commands, tests []string) error
	// GateStorageDir + GateEnforced wire the redundant context-gate
	// satisfaction path: when the gate is enforced and this PostToolUse event
	// is a satisfying anchored tool call, mark the session's gate satisfied so
	// a missed PreToolUse credit can't strand the gate in deny-then-relent.
	// Empty GateStorageDir / false GateEnforced disables it (a no-op).
	GateStorageDir string
	GateEnforced   bool
}

// ExecContexter is the small slice of *sql.DB the hook actually needs:
// inserting an event and querying for a recent duplicate. Every caller passes
// a real *sql.DB, so both methods are always available; tests use an in-memory
// database.
type ExecContexter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// postToolUseDedupWindow is how long two byte-identical tool-call events
// (same session + tool + summary) are collapsed into one. Claude Code can fire
// the same PostToolUse repeatedly during a tight edit/read loop; without this
// the timeline fills with duplicates that add no signal.
const postToolUseDedupWindow = "-5 minutes"

// postToolUseDedupSQL matches a recent non-artifact tool_call with the same
// identifying triple. Uses only existing columns — no schema migration.
const postToolUseDedupSQL = `SELECT 1 FROM session_events
	WHERE session_id = ? AND event_type = 'tool_call' AND tool_name = ? AND summary = ?
	AND created_at > datetime('now', ?) LIMIT 1`

// isRecentDuplicatePostToolUse reports whether an identical tool_call event was
// recorded within the dedup window. Fail-open: any query error returns false so
// the caller still inserts — deduplication must never drop a real event.
func isRecentDuplicatePostToolUse(db ExecContexter, sessionID, toolName, summary string) bool {
	var one int
	err := db.QueryRowContext(context.Background(), postToolUseDedupSQL,
		sessionID, toolName, summary, postToolUseDedupWindow).Scan(&one)
	return err == nil && one == 1
}

// runHookPostToolUse is the production entrypoint: parses flags, opens the
// debug log + lightweight DB context, and delegates the real work to
// recordPostToolUseEvent. Splitting the IO wiring from the recording logic
// lets the latter run end-to-end against a real sqlite3 in tests.
//
// Claude Code delivers the event as JSON on stdin with these canonical
// fields: session_id, hook_event_name, cwd, tool_name, tool_input,
// tool_response. Manual scripts may pass --session-id / --cwd as flag
// fallbacks. Failures NEVER block the upstream tool call.
func runHookPostToolUse(args []string) {
	fs := newFlagSet("hook posttooluse")
	sessionIDFlag := fs.String("session-id", "", "session identifier (fallback when stdin lacks one)")
	configPath := fs.String("config", "", "path to config file")
	cwdFlag := fs.String("cwd", "", "current working directory (fallback when stdin lacks one)")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	// Read stdin once so we can sniff for a Cursor payload before committing to
	// the Claude Code capture path. A Cursor afterFileEdit is translated and
	// driven through the SAME recording pipeline (see hook_cursor.go); Claude
	// Code payloads flow through unchanged (DetectCursorPayload is false).
	content, _ := io.ReadAll(os.Stdin)
	if hookroute.DetectCursorPayload(content) {
		handleCursorHook(os.Stdout, content, *configPath, dlog)
		return
	}

	runPostToolUseWithIO(*configPath, *sessionIDFlag, *cwdFlag, bytes.NewReader(content), os.Stdout, dlog)
}

// artifactCaptureEnabled reports whether tool output should be indexed into
// content_chunks.
//
// This MUST track the flag that gates the Evictor: chunks are written with a
// TTL, but the Evictor that acts on it is constructed only inside the context
// optimizer (pkg/context/optimizer.go, wired from serve.go behind this same
// flag). Capturing while the optimizer is off writes rows nothing reclaims.
func artifactCaptureEnabled(cfg *config.Config) bool {
	return cfg != nil && cfg.ContextOptimizer.Enabled
}

// runPostToolUseWithIO wires the DB / artifact-capture / working-set
// collaborators and runs the recording core against the given stdin/stdout. It
// is split from runHookPostToolUse so the Cursor afterFileEdit path can reuse
// the identical pipeline by feeding a translated Claude-Code-shaped payload.
func runPostToolUseWithIO(configPath, sessionIDFlag, cwdFlag string, stdin io.Reader, stdout io.Writer, dlog *debuglog.Logger) {
	hc, err := openHookContext(configPath)
	if err != nil {
		slog.Warn("posttooluse: hook context init failed", "error", err)
		dlog.Event("hook.posttooluse", map[string]any{"stage": "service_init_failed", "error": err.Error()})
		writePostToolUseResp(stdout, map[string]any{"recorded": false, "reason": "context init failed"})
		return
	}
	defer hc.Close()

	// Wire artifact capture to the content-optimizer store. AddArtifact ->
	// InsertChunk uses prepared statements, so PrepareStatements MUST run
	// first; on failure we leave capture nil and fall back to the event-only
	// path rather than risk a nil-statement panic in the fail-safe hook.
	//
	// Capture is gated on the same flag as eviction — see artifactCaptureEnabled.
	var capture func(projectID, sessionID, artifactType, sourceTool, sourceLabel, content string) (string, error)
	artStore := ctxpkg.NewStore(hc.db, nil)
	if !artifactCaptureEnabled(hc.cfg) {
		dlog.Event("hook.posttooluse", map[string]any{"stage": "artifact_capture_disabled"})
	} else if perr := artStore.PrepareStatements(); perr != nil {
		slog.Warn("posttooluse: prepare statements failed; artifact capture disabled", "error", perr)
		dlog.Event("hook.posttooluse", map[string]any{"stage": "artifact_prepare_failed", "error": perr.Error()})
	} else {
		chunker := ctxpkg.NewChunker(4096)
		capture = func(projectID, sessionID, artifactType, sourceTool, sourceLabel, content string) (string, error) {
			return artStore.AddArtifact(context.Background(), chunker, ctxpkg.ArtifactInput{
				ProjectID: projectID, SessionID: sessionID, Type: artifactType,
				SourceTool: sourceTool, SourceLabel: sourceLabel, Content: content,
			}, 72) // 72h TTL: long enough to debug from, short enough to self-clean
		}
	}

	// Working-set feed: best-effort, shares the hook's DB. A failure to build
	// the manager simply leaves the updater nil (event recording proceeds).
	wsMgr := session.NewManager(hc.db, nil)
	updateWorkingSet := func(sessionID, projectID string, files, commands, tests []string) error {
		_, err := wsMgr.UpdateWorkingSet(context.Background(), sessionID, session.WorkingSetDelta{
			ProjectID: projectID,
			Files:     files,
			Commands:  commands,
			Tests:     tests,
		})
		return err
	}

	recordPostToolUseEvent(PostToolUseDeps{
		Stdin:            stdin,
		Stdout:           stdout,
		DB:               hc.db,
		ResolveProject:   hc.ResolveProject,
		SessionIDFlag:    sessionIDFlag,
		CwdFlag:          cwdFlag,
		Now:              time.Now,
		NewID:            newHookID,
		Logger:           dlog,
		CaptureArtifact:  capture,
		UpdateWorkingSet: updateWorkingSet,
		GateStorageDir:   hc.cfg.Memory.StorageDir,
		GateEnforced:     hc.cfg.Plugin.ContextGateMode() == "enforce",
	})
}

// recordPostToolUseEvent is the testable core: read stdin, decide whether
// to insert, write a JSON status line. All side effects are funneled through
// `deps`; nothing here touches os globals.
func recordPostToolUseEvent(deps PostToolUseDeps) {
	dlog := deps.Logger
	body, err := io.ReadAll(deps.Stdin)
	if err != nil {
		dlog.Event("hook.posttooluse", map[string]any{"stage": "stdin_error", "error": err.Error()})
		writePostToolUseResp(deps.Stdout, map[string]any{"recorded": false, "error": "stdin read failed"})
		return
	}

	var input struct {
		SessionID     string          `json:"session_id"`
		HookEventName string          `json:"hook_event_name"`
		Cwd           string          `json:"cwd"`
		ToolName      string          `json:"tool_name"`
		ToolInput     json.RawMessage `json:"tool_input"`
		ToolResponse  json.RawMessage `json:"tool_response"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			dlog.Event("hook.posttooluse", map[string]any{"stage": "parse_error", "error": err.Error()})
			// Fall through — flag-only invocation is still allowed.
		}
	}

	sessionID := input.SessionID
	if sessionID == "" {
		sessionID = deps.SessionIDFlag
	}
	if sessionID == "" {
		dlog.Event("hook.posttooluse", map[string]any{"stage": "missing_session_id"})
		writePostToolUseResp(deps.Stdout, map[string]any{"recorded": false, "reason": "missing session_id"})
		return
	}

	// Redundant context-gate satisfaction: if the agent just ran a satisfying
	// anchored tool, credit it here too (PreToolUse may have missed it on a
	// stale plugin matcher). Done before the artifact early-return so a large
	// anchored_search response still satisfies the gate. Best-effort.
	if deps.GateEnforced {
		bareTool := input.ToolName
		if i := strings.LastIndex(bareTool, "__"); i >= 0 {
			bareTool = bareTool[i+2:]
		}
		if satisfyGateFromPostToolUse(deps.GateStorageDir, sessionID, bareTool) {
			dlog.Event("hook.posttooluse", map[string]any{"stage": "context_gate_satisfied", "tool": input.ToolName})
		}
	}

	cwdVal := input.Cwd
	if cwdVal == "" {
		cwdVal = deps.CwdFlag
	}
	if cwdVal == "" {
		cwdVal = "."
	}

	projectID := ""
	if deps.ResolveProject != nil {
		projectID = deps.ResolveProject(cwdVal)
	}

	inputText := rawText(input.ToolInput)

	// Feed the working set from this tool call (files edited, commands/tests
	// run) so retrieval can boost in-focus memories. Best-effort: a failure is
	// logged but never blocks the tool call.
	if deps.UpdateWorkingSet != nil {
		files, commands, tests := workingSetDelta(input.ToolName, inputText)
		if len(files)+len(commands)+len(tests) > 0 {
			if wErr := deps.UpdateWorkingSet(sessionID, projectID, files, commands, tests); wErr != nil {
				dlog.Event("hook.posttooluse", map[string]any{"stage": "working_set_update_failed", "error": wErr.Error()})
			}
		}
	}

	// Large tool outputs become searchable artifacts instead of bloating the
	// session-event summary. The event still records that an artifact was
	// captured so the timeline stays complete. Best-effort: a capture failure
	// falls through to the normal event path.
	response := rawText(input.ToolResponse)
	if deps.CaptureArtifact != nil && len(response) > artifactMinBytes {
		atype := classifyArtifact(input.ToolName, inputText, response)
		label := artifactSourceLabel(input.ToolName, inputText)
		if artID, aerr := deps.CaptureArtifact(projectID, sessionID, atype, input.ToolName, label, response); aerr == nil {
			eventID := deps.NewID()
			summary := fmt.Sprintf("captured %s artifact %s (%d bytes) — search with: anchored artifact search", atype, artID, len(response))
			metadata := buildPostToolUseMetadata(cwdVal, input.HookEventName, len(body))
			if _, derr := deps.DB.ExecContext(context.Background(), postToolUseInsertSQL,
				eventID, sessionID, projectID, input.ToolName, summary, metadata); derr != nil {
				dlog.Event("hook.posttooluse", map[string]any{"stage": "artifact_event_insert_failed", "error": derr.Error()})
			}
			dlog.Event("hook.posttooluse", map[string]any{
				"stage": "artifact_captured", "session_id": sessionID, "project_id": projectID,
				"artifact_id": artID, "artifact_type": atype, "bytes": len(response),
			})
			writePostToolUseResp(deps.Stdout, map[string]any{"recorded": true, "artifact_id": artID, "artifact_type": atype})
			return
		} else {
			dlog.Event("hook.posttooluse", map[string]any{"stage": "artifact_capture_failed", "error": aerr.Error()})
			// fall through to event-only path
		}
	}

	summary := summarizeToolEvent(input.ToolResponse, input.ToolInput, 500)

	// Collapse a byte-identical tool call fired again within the dedup window.
	// Fail-open (isRecentDuplicatePostToolUse returns false on any error), so a
	// flaky query can never suppress a genuine event.
	if isRecentDuplicatePostToolUse(deps.DB, sessionID, input.ToolName, summary) {
		dlog.Event("hook.posttooluse", map[string]any{
			"stage": "dedup_skip", "session_id": sessionID, "tool": input.ToolName,
		})
		writePostToolUseResp(deps.Stdout, map[string]any{"recorded": false, "reason": "duplicate"})
		return
	}

	metadata := buildPostToolUseMetadata(cwdVal, input.HookEventName, len(body))
	eventID := deps.NewID()

	_, err = deps.DB.ExecContext(context.Background(), postToolUseInsertSQL,
		eventID, sessionID, projectID, input.ToolName, summary, metadata,
	)
	if err != nil {
		slog.Warn("posttooluse: insert failed", "error", err)
		dlog.Event("hook.posttooluse", map[string]any{"stage": "insert_failed", "error": err.Error()})
		writePostToolUseResp(deps.Stdout, map[string]any{"recorded": false, "reason": "db error"})
		return
	}

	dlog.Event("hook.posttooluse", map[string]any{
		"stage":      "recorded",
		"session_id": sessionID,
		"project_id": projectID,
		"tool":       input.ToolName,
		"event_id":   eventID,
		"summary":    debuglog.Snippet(summary, 200),
	})
	writePostToolUseResp(deps.Stdout, map[string]any{
		"recorded": true,
		"event_id": eventID,
	})
}

// writePostToolUseResp writes a JSON response line to w. Mirrors outputJSON
// but takes an explicit Writer for testability. Marshal failure falls back
// to "{}" so the hook contract (never block) is preserved.
func writePostToolUseResp(w io.Writer, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(w, "{}")
		return
	}
	fmt.Fprintln(w, string(data))
}

// summarizeToolEvent picks the best human-readable summary for the row.
// Tool responses are usually structured (Bash returns {stdout, stderr, ...},
// Read returns {file: ...}). We compact the JSON via encoding/json's stream
// compactor — that preserves key order and number precision, unlike a
// Marshal(Unmarshal()) round-trip. If the response is empty we fall back to
// the input arguments so the row still carries some signal.
//
// Truncation is rune-aware so multibyte sequences (PT-BR/EN/CJK) are never
// split mid-character. `max` is in runes, not bytes.
func summarizeToolEvent(response, input json.RawMessage, max int) string {
	pick := func(raw json.RawMessage) string {
		if len(raw) == 0 || string(raw) == "null" {
			return ""
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			// Not valid JSON — keep raw bytes verbatim, callers prefer
			// imperfect signal over an empty summary.
			return string(raw)
		}
		return buf.String()
	}

	s := pick(response)
	if s == "" {
		s = pick(input)
	}
	if utf8.RuneCountInString(s) > max {
		runes := []rune(s)
		s = string(runes[:max])
	}
	return s
}

func buildPostToolUseMetadata(cwd, hookEvent string, rawLen int) string {
	meta, err := json.Marshal(map[string]any{
		"cwd":             cwd,
		"hook_event_name": hookEvent,
		"raw_length":      rawLen,
	})
	if err != nil {
		return "{}"
	}
	if len(meta) > 1024 {
		meta = meta[:1024]
	}
	return string(meta)
}

// rawText returns the decoded string of a JSON value: a JSON string is
// unquoted to its content (so a Bash tool_response that is a JSON string of
// stdout is measured/classified by its real text), anything else is returned
// as its compact JSON form.
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func newHookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}
