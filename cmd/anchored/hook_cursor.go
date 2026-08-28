package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/hookroute"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/session"
)

// hook_cursor.go routes Cursor hook payloads through the existing anchored hook
// pipeline. Cursor's hooks.json wires the SAME `anchored hook <subcommand>`
// commands as Claude Code; each handler sniffs stdin with
// hookroute.DetectCursorPayload and, on a match, calls handleCursorHook here.
//
// Two contracts differ from Claude Code and are load-bearing:
//   - Wire shape: permission events ("before*") accept
//     {continue, permission: allow|deny|ask, agentMessage} (see
//     FormatDecisionCursor), NOT hookSpecificOutput.permissionDecision.
//   - beforeSubmitPrompt / stop are informational: Cursor IGNORES their stdout,
//     so there is no recall injection or transcript extraction to perform.
//
// Fail-safe contract (identical to the Claude Code handlers): any parse/DB error
// still exits 0, and permission events emit an explicit allow on error so a
// fail-closed Cursor config never blocks the user over an anchored bug.

// cursorPromptWSTimeout bounds the single working-set write done for
// beforeSubmitPrompt so capture never adds meaningful latency to a Cursor prompt.
const cursorPromptWSTimeout = 100 * time.Millisecond

// handleCursorHook is the single entry point for a detected Cursor payload. It
// routes by hook_event_name rather than by which subcommand was invoked, so the
// event is handled correctly regardless of how Cursor's hooks.json is wired.
// The caller MUST return immediately after calling this (it owns the response).
func handleCursorHook(w io.Writer, content []byte, configPath string, dlog *debuglog.Logger) {
	e, err := hookroute.ParseCursorEvent(content)
	if err != nil {
		// DetectCursorPayload already matched the envelope, so a parse failure
		// here is a malformed payload. Emit a safe allow (ignored by the
		// informational events) and exit 0.
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "parse_error", "error": err.Error()})
		emitCursorAllow(w)
		return
	}

	switch e.HookEventName {
	case "beforeShellExecution":
		cursorPreShell(w, e, configPath, dlog)
	case "beforeMCPExecution":
		cursorPreMCP(w, e, configPath, dlog)
	case "afterFileEdit":
		cursorAfterFileEdit(w, e, configPath, dlog)
	case "beforeSubmitPrompt":
		cursorBeforeSubmitPrompt(e, configPath, dlog)
	case "stop":
		cursorStop(w, e, dlog)
	default:
		// Unhandled Cursor events (beforeReadFile, sessionStart/sessionEnd,
		// preToolUse/postToolUse, after*). For an unidentified permission-shaped
		// event an explicit allow is the safe default; the informational events
		// ignore stdout, so it is harmless either way.
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "unhandled_event", "event": e.HookEventName})
		emitCursorAllow(w)
	}
}

// cursorPreShell maps beforeShellExecution to the pretooluse decision logic as
// a Bash tool call, then emits the Cursor permission shape.
func cursorPreShell(w io.Writer, e *hookroute.CursorEvent, configPath string, dlog *debuglog.Logger) {
	d := cursorPreToolDecision(configPath, "Bash", map[string]any{"command": e.Command})
	dlog.Event("hook.cursor", map[string]any{
		"source": "cursor", "stage": "before_shell",
		"session_id": e.SessionID(), "action": cursorActionStr(d),
	})
	emitCursorDecision(w, d)
}

// cursorPreMCP maps beforeMCPExecution to the pretooluse decision logic using
// the raw MCP tool name and decoded tool_input. Cursor sends bare tool names
// like "anchored_execute" (no mcp__anchored__ prefix); the dangerous-pattern
// guard in cursorPreToolDecision strips any prefix to a bare leaf, so the bare
// name matches directly.
func cursorPreMCP(w io.Writer, e *hookroute.CursorEvent, configPath string, dlog *debuglog.Logger) {
	args, err := e.ToolInputMap()
	if err != nil {
		// Malformed tool_input — we cannot inspect it for dangerous patterns;
		// fail-safe to allow rather than block on a decode error.
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "before_mcp_bad_input", "tool": e.ToolName, "error": err.Error()})
		emitCursorAllow(w)
		return
	}
	d := cursorPreToolDecision(configPath, e.ToolName, args)
	dlog.Event("hook.cursor", map[string]any{
		"source": "cursor", "stage": "before_mcp",
		"tool": e.ToolName, "session_id": e.SessionID(), "action": cursorActionStr(d),
	})
	emitCursorDecision(w, d)
}

// cursorPreToolDecision mirrors the Claude Code PreToolUse handler's security
// blocks + routing for a Cursor before* event. It intentionally omits the
// context gate — that is a Claude-Code session-specific mechanism which Cursor
// replaces with an always-on rule file. tool/args are already normalized to the
// Claude Code shape by the callers.
func cursorPreToolDecision(configPath, tool string, args map[string]any) *hookroute.Decision {
	// Security: block writing a secret into a memory/instruction file (Write/Edit
	// only; harmless for the shell/MCP tools Cursor routes here, kept for parity).
	if blocked, reason := memoryFileSecretBlock(tool, args); blocked {
		return &hookroute.Decision{Action: hookroute.ActionDeny, Reason: reason}
	}

	// Security: dangerous-pattern guard for the anchored sandbox tools. The
	// prefix strip leaves a bare "anchored_execute" unchanged, so the bare
	// comparison matches Cursor's un-prefixed tool names.
	bareTool := mcpLeafName(tool)
	if bareTool == "anchored_execute" || bareTool == "anchored_execute_file" || bareTool == "anchored_batch_execute" {
		code, _ := args["code"].(string)
		if bareTool == "anchored_batch_execute" {
			if cmds, ok := args["commands"].([]any); ok {
				for _, cmd := range cmds {
					if m, ok := cmd.(map[string]any); ok {
						if c, ok := m["command"].(string); ok && c != "" {
							code += "\n" + c
						}
					}
				}
			}
		}
		if blocked, pattern := checkDangerousPattern(code); blocked {
			return &hookroute.Decision{Action: hookroute.ActionDeny, Reason: "dangerous pattern detected: " + pattern}
		}
	}

	// Routing: same sandbox redirects as Claude Code, gated on the optimizer so
	// we never deny into a tool that would itself error when it is disabled.
	cfg, _ := loadConfig(configPath)
	optimizerEnabled := cfg != nil && cfg.ContextOptimizer.Enabled
	return hookroute.RoutePreToolUse(tool, args, hookroute.Options{
		OptimizerEnabled: optimizerEnabled,
		SubagentBlock:    mcp.AnchoredSubagentBlock,
	})
}

// cursorAfterFileEdit maps afterFileEdit to the posttooluse capture path by
// translating it to a Claude Code Edit PostToolUse payload and driving the
// identical recording pipeline (project resolve, working-set feed, session
// event insert). tool_name "Edit" lets workingSetDelta record the touched file
// for cross-turn continuity.
func cursorAfterFileEdit(w io.Writer, e *hookroute.CursorEvent, configPath string, dlog *debuglog.Logger) {
	payload, err := json.Marshal(map[string]any{
		"session_id":      e.SessionID(),
		"hook_event_name": "afterFileEdit",
		"cwd":             e.Cwd(),
		"tool_name":       "Edit",
		"tool_input":      map[string]any{"file_path": e.FilePath},
		"tool_response":   map[string]any{"edits": len(e.Edits)},
	})
	if err != nil {
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "after_file_edit_marshal_error", "error": err.Error()})
		writePostToolUseResp(w, map[string]any{"recorded": false})
		return
	}
	dlog.Event("hook.cursor", map[string]any{
		"source": "cursor", "stage": "after_file_edit",
		"session_id": e.SessionID(), "file": e.FilePath, "edits": len(e.Edits),
	})
	// session_id / cwd travel inside the payload; pass them as flag fallbacks too.
	runPostToolUseWithIO(configPath, e.SessionID(), e.Cwd(), bytes.NewReader(payload), w, dlog)
}

// cursorBeforeSubmitPrompt is capture-only. Cursor ignores this hook's stdout,
// so unlike Claude Code's UserPromptSubmit there is no recall injection to emit
// (autoRecallPreview MUST NOT run here). We only feed the session working set
// with the prompt's file/symbol anchors for cross-turn continuity, then emit
// nothing and exit 0.
func cursorBeforeSubmitPrompt(e *hookroute.CursorEvent, configPath string, dlog *debuglog.Logger) {
	sessionID := e.SessionID()
	if sessionID != "" {
		if files, syms := extractAnchors(e.Prompt); len(files)+len(syms) > 0 {
			captureCursorPromptWorkingSet(configPath, sessionID, e.Cwd(), files, syms, dlog)
		}
	}
	dlog.Event("hook.cursor", map[string]any{
		"source": "cursor", "stage": "before_submit_prompt",
		"session_id": sessionID, "prompt_len": len(e.Prompt),
	})
	// Emit nothing: Cursor treats empty stdout as "proceed".
}

// captureCursorPromptWorkingSet merges the prompt's anchors into the session
// working set. Best-effort and bounded: any open/write failure is logged and
// ignored so the prompt is never delayed or blocked.
func captureCursorPromptWorkingSet(configPath, sessionID, cwd string, files, symbols []string, dlog *debuglog.Logger) {
	hc, err := openHookContextWrite(configPath)
	if err != nil {
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "prompt_ws_open_failed", "error": err.Error()})
		return
	}
	defer hc.Close()

	cwdVal := cwd
	if cwdVal == "" {
		cwdVal = "."
	}
	projectID := hc.ResolveProject(cwdVal)

	ctx, cancel := context.WithTimeout(context.Background(), cursorPromptWSTimeout)
	defer cancel()

	mgr := session.NewManager(hc.db, nil)
	if _, err := mgr.UpdateWorkingSet(ctx, sessionID, session.WorkingSetDelta{
		ProjectID: projectID,
		Files:     files,
		Symbols:   symbols,
	}); err != nil {
		dlog.Event("hook.cursor", map[string]any{"source": "cursor", "stage": "prompt_ws_failed", "error": err.Error()})
	}
}

// cursorStop handles the Cursor stop event. Its payload has no transcript_path,
// so the Claude Code stop hook's durable-extraction pass has nothing to read;
// we exit 0 fast with the same {"saved":0} shape. status is logged for probes.
func cursorStop(w io.Writer, e *hookroute.CursorEvent, dlog *debuglog.Logger) {
	dlog.Event("hook.cursor", map[string]any{
		"source": "cursor", "stage": "stop",
		"session_id": e.SessionID(), "status": e.Status,
	})
	writeCursorJSONLine(w, map[string]any{"saved": 0}, "{}")
}

// emitCursorDecision writes a routing decision in Cursor's permission wire
// shape. A nil map (pass/nil decision) becomes an EXPLICIT allow — the safest
// output against a fail-closed Cursor config, and the convention Cursor treats
// as unconditional allow.
func emitCursorDecision(w io.Writer, d *hookroute.Decision) {
	out := hookroute.FormatDecisionCursor(d)
	if out == nil {
		out = map[string]any{"permission": "allow"}
	}
	writeCursorJSONLine(w, out, `{"permission":"allow"}`)
}

// emitCursorAllow writes the explicit-allow response used on parse/decode errors
// and for unhandled permission-shaped events.
func emitCursorAllow(w io.Writer) {
	writeCursorJSONLine(w, map[string]any{"permission": "allow"}, `{"permission":"allow"}`)
}

// writeCursorJSONLine marshals v as a single JSON line, falling back to the
// given literal on marshal failure so the fail-safe contract (never block) holds.
func writeCursorJSONLine(w io.Writer, v any, fallback string) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(w, fallback)
		return
	}
	fmt.Fprintln(w, string(data))
}

// cursorActionStr renders a decision's action for debug logging ("allow" for nil).
func cursorActionStr(d *hookroute.Decision) string {
	if d == nil {
		return "allow"
	}
	if d.Action == hookroute.ActionPass {
		return "allow"
	}
	return string(d.Action)
}
