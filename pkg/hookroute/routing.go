// Package hookroute is the single source of truth for anchored's PreToolUse
// routing: how the agent's native tool calls (Bash/WebFetch/Agent) are steered
// toward anchored's sandbox tools.
//
// It is a Go port of context-mode's hooks/core/routing.mjs, constrained to what
// Claude Code's PreToolUse hook actually supports. The PreToolUse
// hookSpecificOutput schema is strict and accepts ONLY:
//
//	hookEventName, permissionDecision (allow|deny|ask),
//	permissionDecisionReason, updatedInput
//
// There is NO additionalContext channel for PreToolUse (that field exists only
// for UserPromptSubmit / SessionStart / PostToolUse), and permissionDecisionReason
// is surfaced to the model ONLY on deny. So the only ways to influence the model
// at PreToolUse are: deny it (reason shown), or rewrite its input (updatedInput).
// Soft "did you check memory?" nudges have no valid PreToolUse channel — that
// messaging lives in the SessionStart block, the UserPromptSubmit reminder, and
// the skill, which all DO support additionalContext.
//
// What this layer does, therefore:
//   - WebFetch / curl-wget-flood / inline-HTTP / verbose-build-tool → DENY with
//     a redirect to anchored_fetch_and_index / anchored_execute. Gated on
//     OptimizerEnabled: the sandbox tools hard-error when the optimizer is off,
//     so a redirect into one while it's off would be a dead-end — when off we
//     pass through instead.
//   - Agent / Task → MODIFY: prepend the anchored memory routing block to the
//     subagent prompt so subagents use memory too.
//   - Everything else (Read, Grep, Glob, bounded Bash, …) → passthrough.
//
// Why deny (not allow+updatedInput) for Bash redirects: Claude Code ignores
// updatedInput.command under permissionDecision "allow" — the original command
// runs unchanged. Only deny is honored for blocking + surfacing a reason.
package hookroute

import (
	"regexp"
	"strings"
)

// ActionKind is the normalized decision type. FormatDecision maps it to the
// Claude Code PreToolUse wire shape.
type ActionKind string

const (
	ActionPass   ActionKind = ""       // passthrough — let the tool run
	ActionDeny   ActionKind = "deny"   // block the tool, feed Reason to the model
	ActionAsk    ActionKind = "ask"    // prompt the user
	ActionModify ActionKind = "modify" // rewrite tool input (Agent prompt only)
)

// Decision is the normalized routing outcome.
type Decision struct {
	Action       ActionKind
	Reason       string         // deny reason (surfaced to the model)
	UpdatedInput map[string]any // modify payload
}

// Options carries the per-call context the router needs.
type Options struct {
	OptimizerEnabled bool   // context_optimizer.enabled — gates sandbox redirects
	SubagentBlock    string // routing block prepended to Agent prompts
	SessionID        string // reserved (no PreToolUse throttle channel today)
	ProjectDir       string // reserved for future per-project policy
}

// RoutePreToolUse returns a normalized decision for a PreToolUse event, or nil
// for passthrough. It does NOT handle anchored's own security blocks
// (dangerous-pattern in sandbox tools, secret-in-memory-file) — those run in
// the hook before this is called.
func RoutePreToolUse(tool string, toolInput map[string]any, opt Options) *Decision {
	switch canonicalToolName(tool) {
	case "WebFetch":
		return routeWebFetch(toolInput, opt)
	case "Bash":
		return routeBash(toolInput, opt)
	case "Agent":
		return routeAgent(toolInput, opt)
	}
	return nil
}

// deferredToolHint is appended to every sandbox-redirect deny. Claude Code
// often registers the anchored_* tools as DEFERRED, so a direct call to the
// redirect target (anchored_execute / anchored_fetch_and_index) fails as
// not-found — and the agent then tends to abandon anchored entirely, falling
// back to nothing because the original command is blocked. This tells it to
// load the tools once via ToolSearch and keep going instead of giving up.
const deferredToolHint = " If the anchored_* tools are not loaded yet (deferred — a direct call fails as not-found), FIRST run ToolSearch(query: \"anchored_execute anchored_fetch_and_index anchored_ctx_search\") to load them, THEN make the call — do not stop using anchored or silently drop the task just because the schema was not loaded yet."

// ─── WebFetch ───────────────────────────────────────────────────────────────

func routeWebFetch(toolInput map[string]any, opt Options) *Decision {
	if !opt.OptimizerEnabled {
		// Can't redirect into a disabled sandbox — pass through rather than
		// deny into a dead-end.
		return nil
	}
	url, _ := toolInput["url"].(string)
	return &Decision{
		Action: ActionDeny,
		Reason: "anchored: WebFetch redirected. Call anchored_fetch_and_index(url: \"" + url +
			"\", source: \"...\") to fetch + index the page, then anchored_ctx_search(queries: [...]) to query it — the raw page bytes stay out of your context. Full network access; retry the same call on a transient DNS error (EAI_AGAIN, ETIMEDOUT, ENETUNREACH)." + deferredToolHint,
	}
}

// ─── Bash ─────────────────────────────────────────────────────────────────--

func routeBash(toolInput map[string]any, opt Options) *Decision {
	command := shellCommand(toolInput)
	if command == "" {
		return nil
	}
	// Sandbox redirects only make sense when the optimizer is on. When it's off
	// there's nothing to redirect into, so pass everything through.
	if !opt.OptimizerEnabled {
		return nil
	}
	stripped := stripQuotedContent(command)

	// curl/wget that floods stdout → redirect to sandbox.
	if curlWgetFloods(stripped) {
		return &Decision{
			Action: ActionDeny,
			Reason: "anchored: curl/wget redirected. Call anchored_execute(language, code) to fetch the URL, derive your answer in code, and print only the result — the raw HTTP body stays in the sandbox instead of entering your context. Or anchored_fetch_and_index(url, source) when you want to query the response later. Full network access; retry on transient DNS errors (EAI_AGAIN, ETIMEDOUT, ENETUNREACH)." + deferredToolHint,
		}
	}

	// Inline HTTP in a -e/-c snippet (fetch()/requests.get/http.get) → sandbox.
	// Match against noHeredoc (quotes INTACT, only heredoc bodies removed) on
	// purpose: the HTTP call we want to redirect lives inside the quoted script
	// literal of `node -e "fetch(url)"` / `python3 -c "requests.get(url)"`, so
	// stripping quotes would hide the very thing we mean to catch. These
	// patterns are specific enough that innocent quoted text rarely trips them.
	noHeredoc := stripHeredocs(command)
	if inlineHTTPRe.MatchString(noHeredoc) {
		return &Decision{
			Action: ActionDeny,
			Reason: "anchored: inline HTTP redirected. Call anchored_execute(language, code) to fetch, derive your answer in code, and print only the result — the raw response body stays in the sandbox. Full network access; retry on transient DNS errors." + deferredToolHint,
		}
	}

	// Verbose build tools (gradle/maven/sbt) → sandbox tail.
	if buildToolRe.MatchString(stripped) {
		return &Decision{
			Action: ActionDeny,
			Reason: "anchored: build tool redirected. Call anchored_execute(language: \"shell\", code: \"<your build cmd> 2>&1 | tail -30\") so the verbose build log stays in the sandbox and only the tail enters your context. Swap tail for grep -E '(error|warning|FAIL)' to surface only what matters." + deferredToolHint,
		}
	}

	// Everything else passes through. (Structurally-bounded probes never needed
	// a nudge; general commands have no valid soft-nudge channel at PreToolUse.)
	return nil
}

// ─── Agent (subagent prompt injection) ───────────────────────────────────--

// agentPromptFields are the field names different clients use for the subagent
// instruction. We inject the routing block into whichever is present.
var agentPromptFields = []string{"prompt", "request", "objective", "question", "query", "task"}

func routeAgent(toolInput map[string]any, opt Options) *Decision {
	if opt.SubagentBlock == "" {
		return nil
	}
	field := "prompt"
	for _, f := range agentPromptFields {
		if _, ok := toolInput[f]; ok {
			field = f
			break
		}
	}
	prompt, _ := toolInput[field].(string)

	updated := make(map[string]any, len(toolInput)+1)
	for k, v := range toolInput {
		updated[k] = v
	}
	updated[field] = prompt + "\n\n" + opt.SubagentBlock

	return &Decision{Action: ActionModify, UpdatedInput: updated}
}

// ─── Detection helpers ───────────────────────────────────────────────────--

// canonicalToolName normalizes a platform-specific tool name to the canonical
// Claude Code name. Only the aliases anchored's hooks route on are mapped;
// unknowns pass through.
func canonicalToolName(tool string) string {
	if c, ok := toolAliases[tool]; ok {
		return c
	}
	return tool
}

var toolAliases = map[string]string{
	// Gemini CLI / Qwen
	"run_shell_command": "Bash",
	"web_fetch":         "WebFetch",
	// Claude Code legacy name for the subagent tool
	"Task": "Agent",
	// OpenCode
	"bash":  "Bash",
	"fetch": "WebFetch",
	"agent": "Agent",
	// Codex
	"shell":          "Bash",
	"local_shell":    "Bash",
	"container.exec": "Bash",
	// VS Code Copilot / Cursor
	"run_in_terminal": "Bash",
	"Shell":           "Bash",
	// Kiro
	"execute_bash": "Bash",
}

func shellCommand(toolInput map[string]any) string {
	if s, ok := toolInput["command"].(string); ok && s != "" {
		return s
	}
	if s, ok := toolInput["cmd"].(string); ok && s != "" {
		return s
	}
	return ""
}

var (
	inlineHTTPRe = regexp.MustCompile(`(?i)fetch\s*\(\s*['"](https?://|http)|requests\.(get|post|put)\s*\(|http\.(get|request)\s*\(`)
	buildToolRe  = regexp.MustCompile(`(?i)(^|\s|&&|\||;)(\./gradlew|gradlew|gradle|\./mvnw|mvnw|mvn|\./sbt|sbt)(\s|$)`)
	curlWgetRe   = regexp.MustCompile(`(?i)(^|\s|&&|\||;)(curl|wget)\s`)
)

// stripHeredocs removes from a heredoc opener to end-of-string. RE2 has no
// backreference for the closing label, so this is a best-effort scrub for
// false-positive suppression, not a security boundary.
func stripHeredocs(cmd string) string {
	loc := regexp.MustCompile(`<<-?\s*["']?\w+["']?`).FindStringIndex(cmd)
	if loc == nil {
		return cmd
	}
	return cmd[:loc[0]]
}

// stripQuotedContent removes heredocs and quoted strings so token regexes don't
// match inside e.g. `gh issue edit --body "text with curl in it"`.
var singleQuoteRe = regexp.MustCompile(`'[^']*'`)
var doubleQuoteRe = regexp.MustCompile(`"[^"]*"`)

func stripQuotedContent(cmd string) string {
	s := stripHeredocs(cmd)
	s = singleQuoteRe.ReplaceAllString(s, "''")
	s = doubleQuoteRe.ReplaceAllString(s, `""`)
	return s
}

// curlWgetFloods reports whether a curl/wget invocation would flood stdout
// (no file output, or stdout alias, or verbose). Silent file-output downloads
// are allowed. Mirrors context-mode's per-segment evaluation, minus the
// require-silent rule (a plain file download is not a flood).
func curlWgetFloods(stripped string) bool {
	if !curlWgetRe.MatchString(stripped) {
		return false
	}
	segments := regexp.MustCompile(`\s*(?:&&|\|\||;)\s*`).Split(stripped, -1)
	for _, seg := range segments {
		s := strings.TrimSpace(seg)
		if !regexp.MustCompile(`(?i)(^|\s)(curl|wget)\s`).MatchString(s) {
			continue
		}
		isCurl := regexp.MustCompile(`(?i)\bcurl\b`).MatchString(s)
		isWget := regexp.MustCompile(`(?i)\bwget\b`).MatchString(s)

		hasFileOutput := false
		if isCurl {
			hasFileOutput = regexp.MustCompile(`\s(-o|--output)\s`).MatchString(s) || strings.Contains(s, ">")
		} else if isWget {
			hasFileOutput = regexp.MustCompile(`\s(-O|--output-document)\s`).MatchString(s) || strings.Contains(s, ">")
		}
		if !hasFileOutput {
			return true // stdout flood
		}
		// stdout aliases: -o -, -o /dev/stdout, -O - → still floods stdout.
		if isCurl && regexp.MustCompile(`\s(-o|--output)\s+(-|/dev/stdout)(\s|$)`).MatchString(s) {
			return true
		}
		if isWget && regexp.MustCompile(`\s(-O|--output-document)\s+(-|/dev/stdout)(\s|$)`).MatchString(s) {
			return true
		}
		// verbose/trace floods stderr → context.
		if regexp.MustCompile(`\s(-v|--verbose|--trace)\b`).MatchString(s) {
			return true
		}
		// Genuine file destination, no stdout alias, not verbose: nothing of
		// substance reaches context. Allow it.
	}
	return false
}
