package hookroute

// Cursor payload adapter. Cursor's hook JSON shape and its output contract
// are both different from Claude Code's:
//
//   - Input: `hook_event_name` is camelCase (beforeShellExecution, …) and the
//     envelope carries `conversation_id`/`generation_id`/`workspace_roots`
//     instead of Claude Code's `session_id`/`cwd`.
//   - Output: Cursor permission hooks accept
//     {continue, permission: allow|deny|ask, userMessage, agentMessage} —
//     there is no hookSpecificOutput.permissionDecision wrapper, and no
//     updatedInput channel (Cursor has nothing equivalent to CC's Agent-prompt
//     rewrite), so ActionModify has no wire representation here.
//
// This file only translates payload shapes; routing decisions themselves
// still come from RoutePreToolUse / the hook_*.go handlers.
import (
	"encoding/json"
)

// cursorEventNames is the set of hook_event_name values Cursor sends (1.7
// beta docs + newer builds that add sessionStart/sessionEnd/preToolUse/
// postToolUse). Claude Code's hook_event_name is PascalCase (PreToolUse,
// UserPromptSubmit, …) so there is no overlap with this set.
var cursorEventNames = map[string]bool{
	"beforeSubmitPrompt":   true,
	"beforeShellExecution": true,
	"beforeMCPExecution":   true,
	"beforeReadFile":       true,
	"afterFileEdit":        true,
	"stop":                 true,
	"sessionStart":         true,
	"sessionEnd":           true,
	"preToolUse":           true,
	"postToolUse":          true,
	"afterShellExecution":  true,
	"afterMCPExecution":    true,
}

// cursorSniff is the minimal envelope probed to decide whether raw stdin is a
// Cursor payload, without committing to the full CursorEvent parse.
type cursorSniff struct {
	HookEventName  string   `json:"hook_event_name"`
	WorkspaceRoots []string `json:"workspace_roots"`
	ConversationID string   `json:"conversation_id"`
}

// DetectCursorPayload reports whether raw is a Cursor hook payload. The
// discriminator requires BOTH a Cursor-only hook_event_name AND at least one
// of workspace_roots/conversation_id, so a malformed or partial Claude Code
// payload can never cross-match (Claude Code's hook_event_name is PascalCase
// and never appears in cursorEventNames).
func DetectCursorPayload(raw []byte) bool {
	var s cursorSniff
	if err := json.Unmarshal(raw, &s); err != nil {
		return false
	}
	if !cursorEventNames[s.HookEventName] {
		return false
	}
	return len(s.WorkspaceRoots) > 0 || s.ConversationID != ""
}

// CursorAttachment is a beforeSubmitPrompt attachment reference.
type CursorAttachment struct {
	Type     string `json:"type"`
	FilePath string `json:"file_path"`
}

// CursorEdit is a single afterFileEdit change.
type CursorEdit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// CursorEvent is the normalized parse of a Cursor hook stdin payload. All
// fields are optional — which ones are populated depends on hook_event_name.
type CursorEvent struct {
	// Common envelope, present on every Cursor event.
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"`
	HookEventName  string   `json:"hook_event_name"`
	WorkspaceRoots []string `json:"workspace_roots"`

	// beforeSubmitPrompt
	Prompt      string             `json:"prompt"`
	Attachments []CursorAttachment `json:"attachments"`

	// beforeShellExecution. Named ShellCwd (not Cwd) to avoid colliding with
	// the Cwd() method below, which maps workspace_roots[0] instead.
	Command  string `json:"command"`
	ShellCwd string `json:"cwd"`

	// beforeMCPExecution — tool_input arrives as an escaped JSON *string* in
	// today's Cursor beta; ToolInputMap tolerates a raw JSON object too, for
	// forward-compat if Cursor later stops double-encoding it.
	Server    string          `json:"server"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	URL       string          `json:"url"`

	// beforeReadFile
	FilePath string `json:"file_path"`
	Content  string `json:"content"`

	// afterFileEdit (also uses FilePath above)
	Edits []CursorEdit `json:"edits"`

	// stop
	Status string `json:"status"`
}

// ParseCursorEvent unmarshals a raw Cursor hook stdin payload.
func ParseCursorEvent(raw []byte) (*CursorEvent, error) {
	var e CursorEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ToolInputMap decodes ToolInput to a map. Cursor's beforeMCPExecution sends
// tool_input as a JSON string containing escaped JSON (`"{\"k\": \"v\"}"`),
// so the raw bytes must be unmarshaled once to a string and again to a map.
// A raw JSON object is also accepted directly, in case a future Cursor build
// stops double-encoding it.
func (e *CursorEvent) ToolInputMap() (map[string]any, error) {
	if len(e.ToolInput) == 0 {
		return nil, nil
	}

	// Object form: {"k": "v"} — unmarshal directly.
	var asMap map[string]any
	if err := json.Unmarshal(e.ToolInput, &asMap); err == nil {
		return asMap, nil
	}

	// String form: "{\"k\": \"v\"}" — unwrap the string, then unmarshal.
	var asString string
	if err := json.Unmarshal(e.ToolInput, &asString); err != nil {
		return nil, err
	}
	if asString == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(asString), &asMap); err != nil {
		return nil, err
	}
	return asMap, nil
}

// SessionID maps Cursor's conversation_id to anchored's session id concept.
func (e *CursorEvent) SessionID() string {
	return e.ConversationID
}

// Cwd maps Cursor's workspace_roots[0] to anchored's cwd/project-detection
// concept. Empty when no workspace root is present.
func (e *CursorEvent) Cwd() string {
	if len(e.WorkspaceRoots) == 0 {
		return ""
	}
	return e.WorkspaceRoots[0]
}

// FormatDecisionCursor maps a normalized Decision to Cursor's permission hook
// wire shape, or nil for passthrough (the caller then emits a plain allow).
//
// Wire contract (Cursor 1.7+ beta permission hooks — beforeShellExecution,
// beforeMCPExecution, beforeReadFile): the hook's stdout JSON is
// {continue: bool, permission: "allow"|"deny"|"ask", userMessage: string,
// agentMessage: string}. userMessage is shown to the human; agentMessage is
// fed back to the model — the Cursor analog of Claude Code's
// permissionDecisionReason.
//
//   - deny → {"continue": true, "permission": "deny", "agentMessage": Reason}.
//     continue:true lets the agent loop keep running (it just can't perform
//     the denied action), mirroring how CC's deny surfaces a reason without
//     killing the turn.
//   - ask  → {"permission": "ask"}.
//   - pass/nil → nil; caller emits allow (or omits output — Cursor treats a
//     missing decision as unconditional allow, same convention as CC).
//   - modify → nil. Cursor's beforeMCPExecution/beforeShellExecution hooks
//     have no updatedInput-equivalent channel, so ActionModify degrades to
//     pass (used only for the Agent tool, which Cursor doesn't route through
//     this adapter).
func FormatDecisionCursor(d *Decision) map[string]any {
	if d == nil {
		return nil
	}
	switch d.Action {
	case ActionDeny:
		return map[string]any{
			"continue":     true,
			"permission":   "deny",
			"agentMessage": d.Reason,
		}
	case ActionAsk:
		return map[string]any{
			"permission": "ask",
		}
	}
	return nil
}
