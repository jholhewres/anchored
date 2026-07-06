package hookroute

import (
	"testing"
)

// Fixtures below are the documented Cursor 1.7 beta payloads (Cursor docs /
// GitButler hooks deep dive) — same values the real client sends, used
// verbatim so a schema drift in Cursor shows up as a test failure here.
const (
	fixtureBeforeSubmitPrompt = `{"conversation_id":"668320d2-2fd8-4888-b33c-2a466fec86e7","generation_id":"490b90b7-a2ce-4c2c-bb76-cb77b125df2f","prompt":"do something super duper awesome","attachments":[{"type":"file","file_path":"path/to/open/file.rb"}],"hook_event_name":"beforeSubmitPrompt","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureBeforeShellExecution = `{"conversation_id":"668320d2-2fd8-4888-b33c-2a466fec86e7","generation_id":"490b90b7-a2ce-4c2c-bb76-cb77b125df2f","command":"git status","cwd":"","hook_event_name":"beforeShellExecution","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureBeforeMCPExecution = `{"conversation_id":"cdefee2d-2727-4b73-bf77-d9d830f31d2a","generation_id":"63feaa30-ae88-4e47-b6c7-70ee4c39980c","tool_name":"gitbutler_update_branches","tool_input":"{\"changesSummary\": \"Added a README\", \"currentWorkingDirectory\": \"/tmp/x\"}","command":"but","hook_event_name":"beforeMCPExecution","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureBeforeReadFile = `{"conversation_id":"668320d2-2fd8-4888-b33c-2a466fec86e7","generation_id":"490b90b7-a2ce-4c2c-bb76-cb77b125df2f","content":"#!/bin/bash\necho 'my_github_access_token'\n","file_path":"leaks/github_tokens.sh","hook_event_name":"beforeReadFile","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureAfterFileEdit = `{"conversation_id":"cdefee2d-2727-4b73-bf77-d9d830f31d2a","generation_id":"23681cf0-a483-49ab-9748-36044efcef52","file_path":"README.md","edits":[{"old_string":"# OLD README","new_string":"# NEW README"}],"hook_event_name":"afterFileEdit","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureStop = `{"conversation_id":"cdefee2d-2727-4b73-bf77-d9d830f31d2a","generation_id":"26b45fb6-bdea-439c-b2dc-5e97ee00ecea","status":"completed","hook_event_name":"stop","workspace_roots":["/Users/schacon/projects/cc-hooks-example"]}`

	fixtureClaudeCodePreToolUse = `{"tool_name":"Bash","tool_input":{"command":"ls"},"session_id":"abc","hook_event_name":"PreToolUse","cwd":"/tmp"}`

	fixtureClaudeCodeUserPromptSubmit = `{"prompt":"hi","session_id":"abc","hook_event_name":"UserPromptSubmit","cwd":"/tmp"}`

	fixtureGarbage = `not json at all {{{`
)

func TestDetectCursorPayload(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{"beforeSubmitPrompt", fixtureBeforeSubmitPrompt, true},
		{"beforeShellExecution", fixtureBeforeShellExecution, true},
		{"beforeMCPExecution", fixtureBeforeMCPExecution, true},
		{"beforeReadFile", fixtureBeforeReadFile, true},
		{"afterFileEdit", fixtureAfterFileEdit, true},
		{"stop", fixtureStop, true},
		{"claude code PreToolUse", fixtureClaudeCodePreToolUse, false},
		{"claude code UserPromptSubmit", fixtureClaudeCodeUserPromptSubmit, false},
		{"garbage bytes", fixtureGarbage, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectCursorPayload([]byte(tt.raw))
			if got != tt.want {
				t.Errorf("DetectCursorPayload(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestParseCursorEvent_Garbage(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureGarbage))
	if err == nil {
		t.Fatalf("ParseCursorEvent(garbage) expected error, got event %+v", e)
	}
}

func TestParseCursorEvent_BeforeSubmitPrompt(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureBeforeSubmitPrompt))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.ConversationID != "668320d2-2fd8-4888-b33c-2a466fec86e7" {
		t.Errorf("ConversationID = %q", e.ConversationID)
	}
	if e.HookEventName != "beforeSubmitPrompt" {
		t.Errorf("HookEventName = %q", e.HookEventName)
	}
	if e.Prompt != "do something super duper awesome" {
		t.Errorf("Prompt = %q", e.Prompt)
	}
	if len(e.Attachments) != 1 || e.Attachments[0].Type != "file" || e.Attachments[0].FilePath != "path/to/open/file.rb" {
		t.Errorf("Attachments = %+v", e.Attachments)
	}
	if e.SessionID() != e.ConversationID {
		t.Errorf("SessionID() = %q, want %q", e.SessionID(), e.ConversationID)
	}
	if e.Cwd() != "/Users/schacon/projects/cc-hooks-example" {
		t.Errorf("Cwd() = %q", e.Cwd())
	}
}

func TestParseCursorEvent_BeforeShellExecution(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureBeforeShellExecution))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.Command != "git status" {
		t.Errorf("Command = %q", e.Command)
	}
	if e.Cwd() != "/Users/schacon/projects/cc-hooks-example" {
		t.Errorf("Cwd() (from workspace_roots) = %q", e.Cwd())
	}
}

func TestParseCursorEvent_BeforeMCPExecution(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureBeforeMCPExecution))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.ToolName != "gitbutler_update_branches" {
		t.Errorf("ToolName = %q", e.ToolName)
	}
	if e.Command != "but" {
		t.Errorf("Command = %q", e.Command)
	}
	m, err := e.ToolInputMap()
	if err != nil {
		t.Fatalf("ToolInputMap: %v", err)
	}
	if m["changesSummary"] != "Added a README" {
		t.Errorf("ToolInputMap()[changesSummary] = %v", m["changesSummary"])
	}
	if m["currentWorkingDirectory"] != "/tmp/x" {
		t.Errorf("ToolInputMap()[currentWorkingDirectory] = %v", m["currentWorkingDirectory"])
	}
}

func TestParseCursorEvent_BeforeReadFile(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureBeforeReadFile))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.FilePath != "leaks/github_tokens.sh" {
		t.Errorf("FilePath = %q", e.FilePath)
	}
	if e.Content == "" {
		t.Errorf("Content should not be empty")
	}
}

func TestParseCursorEvent_AfterFileEdit(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureAfterFileEdit))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.FilePath != "README.md" {
		t.Errorf("FilePath = %q", e.FilePath)
	}
	if len(e.Edits) != 1 || e.Edits[0].OldString != "# OLD README" || e.Edits[0].NewString != "# NEW README" {
		t.Errorf("Edits = %+v", e.Edits)
	}
}

func TestParseCursorEvent_Stop(t *testing.T) {
	e, err := ParseCursorEvent([]byte(fixtureStop))
	if err != nil {
		t.Fatalf("ParseCursorEvent: %v", err)
	}
	if e.Status != "completed" {
		t.Errorf("Status = %q", e.Status)
	}
}

func TestCursorEvent_SessionIDAndCwdEmpty(t *testing.T) {
	e := &CursorEvent{}
	if e.SessionID() != "" {
		t.Errorf("SessionID() on empty event = %q, want empty", e.SessionID())
	}
	if e.Cwd() != "" {
		t.Errorf("Cwd() on empty event = %q, want empty", e.Cwd())
	}
}

func TestToolInputMap_StringForm(t *testing.T) {
	e := &CursorEvent{ToolInput: []byte(`"{\"k\": \"v\"}"`)}
	m, err := e.ToolInputMap()
	if err != nil {
		t.Fatalf("ToolInputMap: %v", err)
	}
	if m["k"] != "v" {
		t.Errorf("ToolInputMap()[k] = %v, want v", m["k"])
	}
}

func TestToolInputMap_ObjectForm(t *testing.T) {
	// Forward-compat: tolerate tool_input arriving as a raw JSON object,
	// not just the documented escaped-string form.
	e := &CursorEvent{ToolInput: []byte(`{"k": "v"}`)}
	m, err := e.ToolInputMap()
	if err != nil {
		t.Fatalf("ToolInputMap: %v", err)
	}
	if m["k"] != "v" {
		t.Errorf("ToolInputMap()[k] = %v, want v", m["k"])
	}
}

func TestToolInputMap_Empty(t *testing.T) {
	e := &CursorEvent{}
	m, err := e.ToolInputMap()
	if err != nil {
		t.Fatalf("ToolInputMap: %v", err)
	}
	if m != nil {
		t.Errorf("ToolInputMap() on empty ToolInput = %v, want nil", m)
	}
}

func TestToolInputMap_Invalid(t *testing.T) {
	e := &CursorEvent{ToolInput: []byte(`not json`)}
	if _, err := e.ToolInputMap(); err == nil {
		t.Errorf("ToolInputMap() on invalid input expected error, got nil")
	}
}

func TestFormatDecisionCursor(t *testing.T) {
	tests := []struct {
		name string
		d    *Decision
		want map[string]any
	}{
		{
			name: "deny",
			d:    &Decision{Action: ActionDeny, Reason: "blocked: rm -rf /"},
			want: map[string]any{
				"continue":     true,
				"permission":   "deny",
				"agentMessage": "blocked: rm -rf /",
			},
		},
		{
			name: "ask",
			d:    &Decision{Action: ActionAsk},
			want: map[string]any{"permission": "ask"},
		},
		{
			name: "pass",
			d:    &Decision{Action: ActionPass},
			want: nil,
		},
		{
			name: "nil decision",
			d:    nil,
			want: nil,
		},
		{
			name: "modify degrades to pass",
			d:    &Decision{Action: ActionModify, UpdatedInput: map[string]any{"command": "ls"}},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDecisionCursor(tt.d)
			if !mapsEqual(got, tt.want) {
				t.Errorf("FormatDecisionCursor(%+v) = %#v, want %#v", tt.d, got, tt.want)
			}
		})
	}
}

// mapsEqual is a small shallow comparator sufficient for the flat
// map[string]any shapes FormatDecisionCursor produces.
func mapsEqual(a, b map[string]any) bool {
	if a == nil || b == nil {
		return len(a) == len(b)
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || bv != v {
			return false
		}
	}
	return true
}
