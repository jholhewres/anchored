package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/hookroute"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/memory"
)

// runHookPreToolUse inspects an anchored sandbox tool call before execution
// and blocks payloads that contain dangerous patterns. The hook IS registered
// in hooks/hooks.json with a narrow matcher (mcp__anchored__anchored_execute*)
// — the matcher exists because checkDangerousPattern is substring-based and
// would generate false positives if applied to general-purpose Bash. Limiting
// it to the sandbox tools means we only block code the user explicitly asked
// us to execute via anchored, where false positives are easier to reason
// about and the cost of a false negative (rm -rf /, mkfs, dd) is highest.
func runHookPreToolUse(args []string) {
	fs := newFlagSet("hook pretooluse")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		// A stdin read failure is infrastructure, not a security threat —
		// honor the fail-safe contract and allow rather than os.Exit(1), which
		// would abort the user's tool call.
		slog.Error("failed to read stdin", "error", err)
		dlog.Event("hook.pretooluse", map[string]any{"stage": "stdin_error", "error": err.Error()})
		emitAllow()
		return
	}

	// Cursor sends a different envelope (camelCase hook_event_name +
	// workspace_roots/conversation_id) and expects a different output contract.
	// Sniff before the Claude Code parse; on match, route through the Cursor
	// adapter and return. Claude Code payloads never match (PascalCase event
	// names), so the existing path below is byte-for-byte unchanged for them.
	if hookroute.DetectCursorPayload(content) {
		handleCursorHook(os.Stdout, content, *configPath, dlog)
		return
	}

	// Claude Code's canonical PreToolUse payload is {tool_name, tool_input,
	// session_id, hook_event_name, cwd, ...}. Older drafts used {tool,
	// arguments}; we accept either so manual scripts keep working.
	var input struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		SessionID string         `json:"session_id"`
		Cwd       string         `json:"cwd"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		emitAllow()
		return
	}
	tool := input.ToolName
	if tool == "" {
		tool = input.Tool
	}
	args2 := input.ToolInput
	if args2 == nil {
		args2 = input.Arguments
	}

	// Security: block writing a secret into a memory/instruction file. These
	// files (CLAUDE.md, AGENTS.md, .cursor/rules) get committed and shared, so
	// a leaked credential there is high-impact. Memory belongs in anchored
	// (sanitized, never synced raw), so we point the user there instead.
	// Fast path: only Write/Edit reach the body below; the file/secret checks
	// short-circuit on non-memory paths before any real work.
	if blocked, reason := memoryFileSecretBlock(tool, args2); blocked {
		dlog.Event("hook.pretooluse", map[string]any{"stage": "blocked_memory_file_secret", "tool": tool})
		emitDecision(&hookroute.Decision{Action: hookroute.ActionDeny, Reason: reason})
		return
	}

	// Security checks for command execution tools. Claude Code sends the
	// fully-qualified MCP tool name (mcp__anchored__anchored_execute); strip the
	// server prefix to the bare leaf so the match works regardless of wire form.
	bareTool := tool
	if i := strings.LastIndex(bareTool, "__"); i >= 0 {
		bareTool = bareTool[i+2:]
	}
	if bareTool == "anchored_execute" || bareTool == "anchored_execute_file" || bareTool == "anchored_batch_execute" {
		code, _ := args2["code"].(string)
		if bareTool == "anchored_batch_execute" {
			if cmds, ok := args2["commands"].([]any); ok {
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
			dlog.Event("hook.pretooluse", map[string]any{
				"stage":   "blocked",
				"tool":    tool,
				"pattern": pattern,
				"args":    debuglog.Snippet(string(content), 240),
			})
			emitDecision(&hookroute.Decision{
				Action: hookroute.ActionDeny,
				Reason: "dangerous pattern detected: " + pattern,
			})
			return
		}
	}

	// Load config once for both the context gate and the routing/optimizer
	// gating below. A load failure leaves cfg nil — both features then no-op
	// (fail-open), and routing falls back to optimizer-disabled behavior.
	cfg, _ := loadConfig(*configPath)

	// Context gate: optional deterministic enforcement that the agent consult
	// anchored memory (anchored_context/search) before its first substantive
	// tool call in a session. Off by default; only "enforce" arms it. Runs
	// after the security blocks (a gate deny must never pre-empt a security
	// deny) and before routing (a gated tool need not be routed).
	if cfg != nil && cfg.Plugin.ContextGateMode() == "enforce" {
		if dec, stage := contextGateDecision(cfg.Memory.StorageDir, input.SessionID, bareTool, tool); dec != nil {
			dlog.Event("hook.pretooluse", map[string]any{"stage": "context_gate_" + stage, "tool": tool})
			emitDecision(dec)
			return
		} else if stage == "satisfied" {
			dlog.Event("hook.pretooluse", map[string]any{"stage": "context_gate_satisfied", "tool": tool})
		}
	}

	// Routing: steer native exploration tools (Read/Grep/Glob/Bash/WebFetch/
	// Agent and external MCP) toward anchored's memory + sandbox tools. The
	// optimizer flag gates sandbox redirects so we never deny into a tool that
	// would itself error when context_optimizer is disabled.
	optimizerEnabled := cfg != nil && cfg.ContextOptimizer.Enabled
	decision := hookroute.RoutePreToolUse(tool, args2, hookroute.Options{
		OptimizerEnabled: optimizerEnabled,
		SubagentBlock:    mcp.AnchoredSubagentBlock,
		SessionID:        input.SessionID,
		ProjectDir:       input.Cwd,
	})
	if decision != nil {
		dlog.Event("hook.pretooluse", map[string]any{
			"stage":  "routed",
			"tool":   tool,
			"action": string(decision.Action),
		})
		emitDecision(decision)
		return
	}

	dlog.Event("hook.pretooluse", map[string]any{
		"stage": "allow",
		"tool":  tool,
	})
	emitAllow()
}

// emitDecision writes a routing decision in Claude Code's PreToolUse wire
// shape, falling back to a plain allow for passthrough.
func emitDecision(d *hookroute.Decision) {
	out := hookroute.FormatDecision(d)
	if out == nil {
		emitAllow()
		return
	}
	outputJSON(out)
}

// emitAllow writes the passthrough response. It MUST be an empty object, not
// {"decision":"allow"}: Claude Code's hook-output schema types `decision` as
// enum("approve","block"), so "allow" is an invalid VALUE and fails validation
// with "(root): Invalid input" on every passthrough call (the spam this fixes).
// An empty object validates (all fields optional) and means "no opinion — run
// the tool through the normal permission flow". We deliberately do NOT emit
// {"decision":"approve"}, which would auto-approve and bypass permission
// prompts.
func emitAllow() {
	outputJSON(map[string]any{})
}

// memoryFileSecretRe matches the instruction/memory files whose content gets
// committed and shared, so a secret written there leaks widely.
var memoryFileRe = regexp.MustCompile(`(?i)(^|/)(claude\.md|agents\.md)$|\.cursor/rules`)

// hookSecretSanitizer is an always-on secret detector for the pretooluse hook.
// It is independent of the user's sanitizer.enabled config: blocking a secret
// from landing in a shared file is a security floor, not an opt-in.
var hookSecretSanitizer = memory.NewSanitizer(config.SanitizerConfig{Enabled: true})

// memoryFileSecretBlock returns true when a Write/Edit targets a memory or
// instruction file AND the new content contains something the sanitizer would
// redact (a secret/credential). The message points at anchored_save, the safe
// destination. Non-memory paths and clean content fall through fast.
func memoryFileSecretBlock(tool string, args map[string]any) (bool, string) {
	switch tool {
	case "Write", "Edit", "MultiEdit":
	default:
		return false, ""
	}
	path, _ := args["file_path"].(string)
	if path == "" || !memoryFileRe.MatchString(path) {
		return false, ""
	}
	// Content to inspect: Write uses "content"; Edit uses "new_string".
	content, _ := args["content"].(string)
	if content == "" {
		content, _ = args["new_string"].(string)
	}
	if content == "" {
		return false, ""
	}
	if hookSecretSanitizer.Sanitize(content) == content {
		return false, "" // nothing redactable -> no secret
	}
	return true, "writing a credential into " + filepath.Base(path) +
		" would commit it to a shared file. Store it as memory instead — call anchored_save (sanitized, never synced raw), or remove the secret from this edit."
}

func checkDangerousPattern(code string) (blocked bool, pattern string) {
	dangerous := []string{
		"rm -rf /",
		"rm -rf /*",
		":(){:|:&};:",
		"dd if=/dev/zero",
		"mkfs",
		"format c:",
		"curl",
		"wget",
		"nc -l",
	}
	lower := strings.ToLower(code)
	for _, d := range dangerous {
		if strings.Contains(lower, strings.ToLower(d)) {
			// Fine-grained: curl/wget only block if piping to shell
			if d == "curl" || d == "wget" {
				if strings.Contains(lower, "|") && (strings.Contains(lower, "sh") || strings.Contains(lower, "bash")) {
					return true, d + " piped to shell"
				}
				continue
			}
			return true, d
		}
	}
	return false, ""
}

// outputJSON writes a hook response as a single JSON line. Hook handlers
// promise to never block the upstream tool call (`os.Exit(1)` would do exactly
// that on Claude Code), so a marshal failure falls back to "{}" rather than
// terminating the process. Marshal errors here are practically unreachable —
// callers always pass map[string]any with primitive values — but the fallback
// keeps the contract honest.
func outputJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Warn("hook: outputJSON marshal failed; emitting empty object", "error", err)
		fmt.Println("{}")
		return
	}
	fmt.Println(string(data))
}
