package main

import (
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/hookroute"

	_ "github.com/mattn/go-sqlite3"
)

// Cursor payload fixtures. These mirror the documented Cursor 1.7 beta payloads
// used verbatim in pkg/hookroute/cursor_test.go (redefined here because those
// consts are unexported to the hookroute package).
const (
	curBeforeShell = `{"conversation_id":"conv-shell","generation_id":"g1","command":"git status","cwd":"","hook_event_name":"beforeShellExecution","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	// beforeMCPExecution with the anchored sandbox tool (bare name, no prefix)
	// and a dangerous pattern in the escaped-JSON-string tool_input Cursor sends.
	curBeforeMCPDangerous = `{"conversation_id":"conv-mcp","generation_id":"g1","tool_name":"anchored_execute","tool_input":"{\"language\": \"shell\", \"code\": \"rm -rf /\"}","hook_event_name":"beforeMCPExecution","workspace_roots":["/tmp/x"]}`

	curAfterFileEdit = `{"conversation_id":"conv-edit","generation_id":"g1","file_path":"README.md","edits":[{"old_string":"# OLD","new_string":"# NEW"}],"hook_event_name":"afterFileEdit","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	curBeforeSubmitPrompt = `{"conversation_id":"conv-prompt","generation_id":"g1","prompt":"do something super duper awesome","hook_event_name":"beforeSubmitPrompt","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	curStop = `{"conversation_id":"conv-stop","generation_id":"g1","status":"completed","hook_event_name":"stop","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	// A Claude Code PreToolUse that must NOT be detected as Cursor and must keep
	// producing the hookSpecificOutput deny shape (secret into CLAUDE.md).
	ccWriteSecret = `{"tool_name":"Write","tool_input":{"file_path":"/repo/CLAUDE.md","content":"deploy key is AKIA1234567890ABCDEF"},"session_id":"s1","hook_event_name":"PreToolUse","cwd":"/repo"}`

	curGarbage = `not json at all {{{`
)

// TestCursorPreShell_AllowsPlainCommand: a benign shell command yields an
// explicit Cursor allow (permission field present, not a deny).
func TestCursorPreShell_AllowsPlainCommand(t *testing.T) {
	cfgPath := newTestEnv(t)
	e, err := hookroute.ParseCursorEvent([]byte(curBeforeShell))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf strings.Builder
	cursorPreShell(&buf, e, cfgPath, nilDebugLogger())

	out := buf.String()
	if !strings.Contains(out, `"permission"`) {
		t.Fatalf("expected a permission field, got %q", out)
	}
	if !strings.Contains(out, `"allow"`) {
		t.Fatalf("expected allow, got %q", out)
	}
	if strings.Contains(out, `"deny"`) {
		t.Fatalf("benign command must not be denied, got %q", out)
	}
}

// TestCursorPreMCP_DeniesDangerousPattern: beforeMCPExecution routing anchored_
// execute with "rm -rf /" in the tool_input yields a Cursor deny.
func TestCursorPreMCP_DeniesDangerousPattern(t *testing.T) {
	cfgPath := newTestEnv(t)
	e, err := hookroute.ParseCursorEvent([]byte(curBeforeMCPDangerous))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf strings.Builder
	cursorPreMCP(&buf, e, cfgPath, nilDebugLogger())

	out := buf.String()
	if !strings.Contains(out, `"permission":"deny"`) {
		t.Fatalf("expected permission deny, got %q", out)
	}
	if !strings.Contains(out, `"continue":true`) {
		t.Fatalf("deny should keep the agent loop running (continue:true), got %q", out)
	}
	if !strings.Contains(out, "rm -rf") {
		t.Fatalf("deny agentMessage should name the pattern, got %q", out)
	}
}

// TestCursorAfterFileEdit_RecordsEvent: afterFileEdit records a session_events
// row via the shared posttooluse pipeline, keyed by conversation_id, with the
// workspace root carried into the event metadata.
func TestCursorAfterFileEdit_RecordsEvent(t *testing.T) {
	cfgPath := newTestEnv(t)

	// Migrate the DB at the config's path so openHookContext sees the schema.
	_, _, svc, err := initService(cfgPath)
	if err != nil {
		t.Fatalf("initService: %v", err)
	}
	svc.Close()

	e, err := hookroute.ParseCursorEvent([]byte(curAfterFileEdit))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf strings.Builder
	cursorAfterFileEdit(&buf, e, cfgPath, nilDebugLogger())

	if !strings.Contains(buf.String(), `"recorded":true`) {
		t.Fatalf("expected recorded=true, got %q", buf.String())
	}

	// Reopen the DB directly and assert the row.
	db := openCursorTestDB(t, cfgPath)
	var toolName, sessionID, metadata string
	err = db.QueryRow(
		`SELECT tool_name, session_id, metadata FROM session_events WHERE session_id = ?`,
		"conv-edit",
	).Scan(&toolName, &sessionID, &metadata)
	if err != nil {
		t.Fatalf("query session_events: %v", err)
	}
	if toolName != "Edit" {
		t.Errorf("tool_name = %q, want Edit", toolName)
	}
	if sessionID != "conv-edit" {
		t.Errorf("session_id = %q, want conv-edit", sessionID)
	}
	if !strings.Contains(metadata, "cc-hooks-example") {
		t.Errorf("metadata should carry workspace_roots[0] as cwd, got %q", metadata)
	}
}

// TestHandleCursorHook_BeforeSubmitPrompt_EmitsNothing: the capture-only prompt
// event writes no output (Cursor ignores it) and never errors.
func TestHandleCursorHook_BeforeSubmitPrompt_EmitsNothing(t *testing.T) {
	cfgPath := newTestEnv(t)
	var buf strings.Builder
	handleCursorHook(&buf, []byte(curBeforeSubmitPrompt), cfgPath, nilDebugLogger())
	if buf.String() != "" {
		t.Fatalf("beforeSubmitPrompt must emit nothing, got %q", buf.String())
	}
}

// TestHandleCursorHook_Stop_ExitsZero: the stop event emits the {"saved":0}
// shape and does not attempt transcript extraction.
func TestHandleCursorHook_Stop_ExitsZero(t *testing.T) {
	cfgPath := newTestEnv(t)
	var buf strings.Builder
	handleCursorHook(&buf, []byte(curStop), cfgPath, nilDebugLogger())
	out := buf.String()
	if !strings.Contains(out, `"saved":0`) {
		t.Fatalf("expected saved:0, got %q", out)
	}
}

// TestHandleCursorHook_Garbage: a payload that fails to parse (yet somehow
// reached the adapter) emits a safe allow, never a non-zero exit.
func TestHandleCursorHook_Garbage(t *testing.T) {
	cfgPath := newTestEnv(t)
	var buf strings.Builder
	handleCursorHook(&buf, []byte(curGarbage), cfgPath, nilDebugLogger())
	if !strings.Contains(buf.String(), `"permission":"allow"`) {
		t.Fatalf("garbage must fail-safe to allow, got %q", buf.String())
	}
}

// TestRunHookPreToolUse_ClaudeCodeRegression proves the Claude Code PreToolUse
// path is unchanged: a CC payload is NOT routed through the Cursor adapter and
// still emits the hookSpecificOutput deny shape (no "permission" key).
func TestRunHookPreToolUse_ClaudeCodeRegression(t *testing.T) {
	cfgPath := newTestEnv(t)
	out := runHandlerWithStdin(t, func(args []string) { runHookPreToolUse(args) },
		[]string{"--config", cfgPath}, ccWriteSecret)

	if !strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("CC path must keep hookSpecificOutput shape, got %q", out)
	}
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("CC secret write should still deny, got %q", out)
	}
	if strings.Contains(out, `"permission":"allow"`) || strings.Contains(out, `"permission":"deny"`) {
		t.Fatalf("CC path must NOT emit Cursor permission shape, got %q", out)
	}
}

// TestRunHookPreToolUse_CursorShell_EndToEnd proves the full pretooluse handler
// sniffs a Cursor beforeShellExecution payload from stdin and emits the Cursor
// permission shape.
func TestRunHookPreToolUse_CursorShell_EndToEnd(t *testing.T) {
	cfgPath := newTestEnv(t)
	out := runHandlerWithStdin(t, func(args []string) { runHookPreToolUse(args) },
		[]string{"--config", cfgPath}, curBeforeShell)

	if !strings.Contains(out, `"permission"`) {
		t.Fatalf("expected Cursor permission shape, got %q", out)
	}
	if strings.Contains(out, "hookSpecificOutput") {
		t.Fatalf("Cursor path must not emit the Claude Code shape, got %q", out)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// openCursorTestDB opens the migrated DB at the config's database_path for
// direct assertions.
func openCursorTestDB(t *testing.T, cfgPath string) *sql.DB {
	t.Helper()
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	db, err := sql.Open("sqlite3", cfg.Memory.DatabasePath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// runHandlerWithStdin redirects os.Stdin/os.Stdout around a hook handler
// invocation and returns the captured stdout. Handlers read os.Stdin directly,
// so this is the only way to exercise them end-to-end in-process.
func runHandlerWithStdin(t *testing.T, fn func([]string), args []string, stdin string) string {
	t.Helper()
	oldIn, oldOut := os.Stdin, os.Stdout
	rIn, wIn, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe in: %v", err)
	}
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe out: %v", err)
	}
	os.Stdin = rIn
	os.Stdout = wOut
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()

	go func() {
		_, _ = io.WriteString(wIn, stdin)
		wIn.Close()
	}()

	done := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(rOut)
		done <- string(data)
	}()

	fn(args)
	wOut.Close()
	return <-done
}
