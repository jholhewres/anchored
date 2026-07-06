# IDE / Tool Configs

Sample configs for clients that don't auto-install hooks via the Claude Code plugin manifest.

For **Claude Code**, just install the plugin (`/plugin install anchored@anchored`) — the SessionStart, UserPromptSubmit, PostToolUse, and PreCompact hooks are wired by `hooks/hooks.json` automatically.

For everything else, the snippets below register the `anchored` MCP server **and** the routing-block hooks so the agent treats anchored as the persistent memory layer instead of waiting for explicit instructions.

## Cursor

Cursor's hook schema differs from Claude Code's — event names are
`beforeShellExecution`, `beforeMCPExecution`, `afterFileEdit`, `beforeSubmitPrompt`,
`stop` (not `preToolUse`/`postToolUse`/`SessionStart`). `anchored init --tool cursor`
installs all three files below for you; the manual steps are only needed without `init`.

1. Drop the contents of [`cursor/mcp.json`](cursor/mcp.json) into `~/.cursor/mcp.json` (merge with existing `mcpServers`).
2. Drop the contents of [`cursor/hooks.json`](cursor/hooks.json) into `~/.cursor/hooks.json` (or `<project>/.cursor/hooks.json` for project-scoped), replacing `<BIN>` with the absolute path to your `anchored` binary.
3. Drop [`cursor/anchored.mdc`](cursor/anchored.mdc) into `~/.cursor/rules/anchored.mdc`.
4. Restart Cursor.

`beforeShellExecution`/`beforeMCPExecution` route through the pretooluse guard
(dangerous-pattern and secret-write checks); `afterFileEdit` and `stop` capture session
events for continuity. **`beforeSubmitPrompt` is informational-only in Cursor** — unlike
Claude Code's `UserPromptSubmit`, Cursor ignores any JSON a hook returns from this event,
so it cannot inject memory context into the prompt. Activation instead comes from
`anchored.mdc`, an always-on rule (`alwaysApply: true`) instructing the agent to call
`anchored_context`/`anchored_search`/`anchored_save` proactively — the role Claude
Code's SessionStart/UserPromptSubmit hooks play there.

### Probing real Cursor hook payloads

Cursor's hooks API is in beta and documented event/field names may drift. To see
exactly what Cursor sends before relying on it (or to debug a hook that isn't firing),
use [`cursor/probe-hooks.json`](cursor/probe-hooks.json): every covered event runs
`sh -c 'cat >> ~/.anchored/cursor-probe.log'`, appending the raw stdin payload with no
stdout output, so it can never block Cursor.

1. Back up your real hooks.json: `cp ~/.cursor/hooks.json ~/.cursor/hooks.json.bak`.
2. Install the probe: `cp cursor/probe-hooks.json ~/.cursor/hooks.json`.
3. Use Cursor briefly — send a prompt, edit a file, run a shell command, stop a
   generation — to trigger each event.
4. Inspect `~/.anchored/cursor-probe.log` for the real payload shapes.
5. Restore the real hooks.json: `cp ~/.cursor/hooks.json.bak ~/.cursor/hooks.json`
   (or re-run `anchored init --tool cursor`).

## OpenCode

1. Merge [`opencode/opencode.json`](opencode/opencode.json) into your `~/.config/opencode/opencode.json` (or `opencode.json` at the repo root).
2. Restart OpenCode.

OpenCode does not yet expose a stable `SessionStart` event, so `experimental.chat.system.transform` is used as the surrogate — anchored injects the routing block into the system prompt at session start. `experimental.hook.chat_message` re-injects on every user prompt; `experimental.hook.session_compacting` snapshots before compaction.

## Antigravity (agy)

1. Merge [`agy/mcp_config.json`](agy/mcp_config.json) into your `~/.gemini/config/mcp_config.json` (Antigravity 2.0 desktop) or `~/.gemini/antigravity-cli/mcp_config.json` (Antigravity CLI).
2. Restart Antigravity.

Antigravity does not yet expose a hook system, so the MCP tool descriptions and the `Instructions` field returned in `initialize` steer the model. You may need to ask the agent to "check anchored memory" occasionally.

## Anything else MCP-compatible

If your tool only supports MCP server registration (no hooks), just register `anchored` as the MCP server and the tool descriptions + the `Instructions` field returned in `initialize` will steer the model. You won't get the SessionStart/UserPromptSubmit reminders, so the model may need a nudge ("check anchored memory") more often.
