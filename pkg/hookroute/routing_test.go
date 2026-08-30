package hookroute

import (
	"strings"
	"testing"
)

func optOn() Options  { return Options{OptimizerEnabled: true, SubagentBlock: "BLOCK"} }
func optOff() Options { return Options{OptimizerEnabled: false, SubagentBlock: "BLOCK"} }

func TestWebFetchRedirectsWhenOptimizerOn(t *testing.T) {
	d := RoutePreToolUse("WebFetch", map[string]any{"url": "https://example.com"}, optOn())
	if d == nil || d.Action != ActionDeny {
		t.Fatalf("WebFetch should deny+redirect when optimizer on, got %+v", d)
	}
	if !strings.Contains(d.Reason, "anchored_fetch_and_index") {
		t.Errorf("deny reason must point to anchored_fetch_and_index: %q", d.Reason)
	}
}

func TestWebFetchPassesThroughWhenOptimizerOff(t *testing.T) {
	d := RoutePreToolUse("WebFetch", map[string]any{"url": "https://example.com"}, optOff())
	if d != nil {
		t.Fatalf("WebFetch must pass through (not deny into a disabled sandbox), got %+v", d)
	}
}

func TestCurlStdoutFloodRedirects(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": "curl https://api.example.com/data"}, optOn())
	if d == nil || d.Action != ActionDeny {
		t.Fatalf("curl stdout flood should deny+redirect, got %+v", d)
	}
	if !strings.Contains(d.Reason, "anchored_execute") {
		t.Errorf("reason must mention anchored_execute: %q", d.Reason)
	}
}

func TestCurlSilentFileDownloadAllowed(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": "curl -s -o out.bin https://example.com/f"}, optOn())
	if d != nil {
		t.Fatalf("silent file download must pass through, got %+v", d)
	}
}

func TestCurlFileDownloadWithoutSilentAllowed(t *testing.T) {
	// Genuine file destination, no stdout alias, not verbose → not a flood.
	d := RoutePreToolUse("Bash", map[string]any{"command": "curl -o report.json https://api.example.com/data"}, optOn())
	if d != nil && d.Action == ActionDeny {
		t.Fatalf("plain file download must not be denied, got %+v", d)
	}
}

func TestCurlStdoutAliasStillFloods(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": "curl -o - https://api.example.com/data"}, optOn())
	if d == nil || d.Action != ActionDeny {
		t.Fatalf("curl -o - is a stdout alias → must deny, got %+v", d)
	}
}

func TestCurlInsideQuotesNotFlagged(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": `gh issue edit 1 --body "use curl to fetch"`}, optOn())
	if d != nil {
		t.Fatalf("curl inside a quoted string must pass through, got %+v", d)
	}
}

func TestInlineHTTPInScriptLiteralRedirects(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": `node -e "fetch('https://api.example.com/x')"`}, optOn())
	if d == nil || d.Action != ActionDeny {
		t.Fatalf("inline HTTP in a -e literal should deny+redirect, got %+v", d)
	}
}

func TestBuildToolRedirects(t *testing.T) {
	d := RoutePreToolUse("Bash", map[string]any{"command": "gradle build"}, optOn())
	if d == nil || d.Action != ActionDeny {
		t.Fatalf("gradle build should deny+redirect, got %+v", d)
	}
}

func TestBashPassesThroughWhenOptimizerOff(t *testing.T) {
	// No sandbox to redirect into → never deny, regardless of command.
	for _, cmd := range []string{"curl https://x/data", "gradle build", "ls"} {
		if d := RoutePreToolUse("Bash", map[string]any{"command": cmd}, optOff()); d != nil {
			t.Errorf("optimizer off: %q must pass through, got %+v", cmd, d)
		}
	}
}

func TestOrdinaryBashPassesThrough(t *testing.T) {
	for _, cmd := range []string{"pwd", "git status", "ls -la", "go test ./...", "grep -r foo ."} {
		if d := RoutePreToolUse("Bash", map[string]any{"command": cmd}, optOn()); d != nil {
			t.Errorf("%q has no valid PreToolUse nudge channel; want passthrough, got %+v", cmd, d)
		}
	}
}

func TestReadGrepGlobPassThrough(t *testing.T) {
	// PreToolUse has no additionalContext channel — these must never emit a
	// decision (that's what caused the "Invalid input" hook errors).
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		if d := RoutePreToolUse(tool, map[string]any{"file_path": "/x", "pattern": "y"}, optOn()); d != nil {
			t.Errorf("%s must pass through, got %+v", tool, d)
		}
	}
}

func TestExternalMCPPassesThrough(t *testing.T) {
	if d := RoutePreToolUse("mcp__slack__post", map[string]any{}, optOn()); d != nil {
		t.Errorf("external MCP has no valid PreToolUse nudge channel; want passthrough, got %+v", d)
	}
}

func TestAgentPromptInjection(t *testing.T) {
	d := RoutePreToolUse("Agent", map[string]any{"prompt": "do the thing", "subagent_type": "general-purpose"}, optOn())
	if d == nil || d.Action != ActionModify {
		t.Fatalf("Agent should modify the prompt, got %+v", d)
	}
	got, _ := d.UpdatedInput["prompt"].(string)
	if !strings.Contains(got, "do the thing") || !strings.Contains(got, "BLOCK") {
		t.Errorf("modified prompt must keep original and append the block: %q", got)
	}
	if d.UpdatedInput["subagent_type"] != "general-purpose" {
		t.Error("other fields must be preserved")
	}
}

func TestAgentTaskAliasInjects(t *testing.T) {
	d := RoutePreToolUse("Task", map[string]any{"prompt": "x"}, optOn())
	if d == nil || d.Action != ActionModify {
		t.Fatalf("Task (legacy Agent) should modify the prompt, got %+v", d)
	}
}

func TestAnchoredOwnToolsPassThrough(t *testing.T) {
	if d := RoutePreToolUse("mcp__anchored__anchored_save", map[string]any{}, optOn()); d != nil {
		t.Errorf("anchored's own tools must pass through, got %+v", d)
	}
}

func TestFormatDecisionWireShapes(t *testing.T) {
	deny := FormatDecision(&Decision{Action: ActionDeny, Reason: "no"})
	hso, _ := deny["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" || hso["permissionDecisionReason"] != "no" {
		t.Errorf("deny wire shape wrong: %+v", deny)
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Errorf("missing hookEventName: %+v", deny)
	}
	mod := FormatDecision(&Decision{Action: ActionModify, UpdatedInput: map[string]any{"prompt": "x"}})
	hso2, _ := mod["hookSpecificOutput"].(map[string]any)
	if _, ok := hso2["updatedInput"]; !ok {
		t.Errorf("modify wire shape must carry updatedInput: %+v", mod)
	}
	// No PreToolUse output must ever carry additionalContext (the bug we fixed).
	for _, m := range []map[string]any{deny, mod} {
		if h, ok := m["hookSpecificOutput"].(map[string]any); ok {
			if _, bad := h["additionalContext"]; bad {
				t.Error("PreToolUse output must never include additionalContext")
			}
		}
	}
	if FormatDecision(nil) != nil {
		t.Error("nil decision must format to nil (passthrough)")
	}
}

// The redirect deny hint must never hardcode a fully-qualified MCP tool name:
// registration prefixes drift across harnesses (mcp__anchored__*,
// mcp__plugin_anchored_anchored__*), and a ToolSearch select: with a stale FQN
// loads nothing — the agent then abandons the redirect target entirely.
func TestDeferredToolHintHasNoHardcodedFQN(t *testing.T) {
	if strings.Contains(deferredToolHint, "select:mcp__anchored__") {
		t.Error("deferredToolHint hardcodes an MCP FQN — search by leaf name instead")
	}
	if !strings.Contains(deferredToolHint, "ToolSearch") {
		t.Error("deferredToolHint should still point at ToolSearch")
	}
}
