# Connect your tools

## Claude Code plugin

The plugin is the easiest path because it installs MCP registration, slash commands, hooks, and the auto-trigger skill together:

```text
/plugin marketplace add jholhewres/anchored
/plugin install anchored@anchored
```

Restart Claude Code after installation. Available slash commands include `/anchored:context`, `/anchored:search`, `/anchored:save`, `/anchored:stats`, `/anchored:doctor`, and `/anchored:purge`.

> **Running context-mode too?** Anchored now ships its own PreToolUse routing —
> it steers Read/Grep/Glob/Bash/WebFetch and subagents toward memory and the
> sandbox tools, the same mechanism context-mode uses. Running both plugins
> means two routing blocks competing for the model's attention, and
> context-mode's redirects can shadow Anchored's. Uninstall context-mode for the
> cleanest behavior — Anchored covers both the memory and the context-window
> story on its own.

## MCP only

```bash
claude mcp add -s user anchored anchored
```

The `-s user` flag makes Anchored available in every project. Without it, Claude Code registers the server only for the current project.

## Other tools

Run auto-detection:

```bash
anchored init
```

Or target one tool:

```bash
anchored init --tool cursor
anchored init --tool opencode
anchored init --tool gemini
anchored init --tool agy
anchored init --tool windsurf
anchored init --tool cline
anchored init --tool vscode --cwd /path/to/project
anchored init --tool codex
anchored init --tool devin
anchored init --tool openclaw
anchored init --tool hermes
anchored init --tool devclaw
anchored init --tool gatorclaw
anchored init --tool pi
```

Supported config locations:

| Tool | Config file | Format |
|---|---|---|
| Claude Code | `~/.claude.json` | JSON `mcpServers` |
| Cursor | `~/.cursor/mcp.json` | JSON `mcpServers` |
| OpenCode | `~/.config/opencode/opencode.json` | JSON `mcpServers` |
| Gemini CLI | `~/.gemini/settings.json` | JSON `mcpServers` |
| Antigravity 2.0 | `~/.gemini/config/mcp_config.json` | JSON `mcpServers` |
| Antigravity CLI (`agy`) | `~/.gemini/antigravity-cli/mcp_config.json` | JSON `mcpServers` |
| Windsurf | `~/.codeium/windsurf/mcp_config.json` | JSON `mcpServers` |
| Cline | `~/.cline/mcp.json` | JSON `mcpServers` |
| VS Code Copilot | `.vscode/mcp.json` | JSON `servers` |
| Codex CLI | `~/.codex/config.toml` | TOML |
| Devin | `.devin/config.json` | JSON `mcpServers` |
| OpenClaw | `~/.openclaw/openclaw.json` | JSON `mcpServers` |
| Hermes | `~/.hermes/config.yaml` | YAML `mcp_servers` |
| devclaw | `~/.devclaw/config.yaml` | YAML `mcp.servers[]` |
| gatorclaw | `~/.gatorclaw/config.yaml` | YAML `mcp.servers[]` |
| pi | `~/.pi/agent/extensions/anchored/` | TS extension (no MCP) |

For hosts with a memory-plugin slot, `init` also installs a **deep plugin** that
bridges the agent lifecycle to the local `anchored` binary (recall via
`anchored search`, capture via `anchored save`) — no daemon:

| Host | Deep plugin |
|---|---|
| OpenClaw | `~/.openclaw/extensions/anchored/` + `plugins.slots.memory = anchored` |
| Hermes | `~/.hermes/plugins/anchored/` + `memory.provider = anchored` |
| pi | `~/.pi/agent/extensions/anchored/index.ts` (pi has no MCP surface) |

Every registration preserves foreign config, backs up to `.bak`, and is
idempotent.

Most tools use this JSON shape:

```json
{
  "mcpServers": {
    "anchored": {
      "command": "anchored"
    }
  }
}
```

VS Code Copilot uses `servers` and requires `type: "stdio"`:

```json
{
  "servers": {
    "anchored": {
      "type": "stdio",
      "command": "anchored"
    }
  }
}
```

Codex CLI uses TOML:

```toml
[mcp_servers.anchored]
command = "anchored"
enabled = true
```

## VS Code Copilot

> **Note:** Hooks are in Preview in VS Code. See the
> [official documentation](https://code.visualstudio.com/docs/agent-customization/hooks)
> for details.

### 1. MCP server

Register Anchored as an MCP server in the current project:

```bash
mkdir -p .vscode
anchored init --tool vscode --cwd /path/to/project
```

To make Anchored available in **all projects**, use the user profile's
global configuration file. Open the file via the `MCP: Open User Configuration`
command and add it manually:

```json
{
  "servers": {
    "anchored": {
      "type": "stdio",
      "command": "anchored"
    }
  }
}
```

> **Notes:**
> - Make sure the `anchored` command is available in your terminal's PATH —
>   VS Code runs it directly when starting the server.
> - On first run, VS Code displays a trust dialog — confirm to activate
>   Anchored's tools.

### 2. Hooks (automatic context injection)

VS Code Copilot supports hooks using the same format as Claude Code, including
`SessionStart`, `UserPromptSubmit`, `PostToolUse`, `PreCompact`, and `Stop`.
With hooks, `anchored_context` is automatically injected at the start of every
conversation — no manual call needed.

Create `.github/hooks/anchored.json` in your project:

```json
{
  "hooks": {
    "SessionStart": [
      {
        "type": "command",
        "command": "anchored hook sessionstart",
        "timeout": 15
      }
    ],
    "UserPromptSubmit": [
      {
        "type": "command",
        "command": "anchored hook userpromptsubmit",
        "timeout": 10
      }
    ],
    "PostToolUse": [
      {
        "type": "command",
        "command": "anchored hook posttooluse",
        "timeout": 10
      }
    ],
    "PreCompact": [
      {
        "type": "command",
        "command": "anchored hook precompact",
        "timeout": 10
      }
    ],
    "Stop": [
      {
        "type": "command",
        "command": "anchored hook stop",
        "timeout": 10
      }
    ]
  }
}
```

VS Code also accepts hooks from the following locations:

| Location | Scope |
|---|---|
| `.github/hooks/*.json` | Workspace |
| `.claude/settings.json` | Workspace |
| `.claude/settings.local.json` | Workspace local |
| `~/.claude/settings.json` | User (global) |

After saving, VS Code loads the hooks automatically. On the next conversation,
`SessionStart` will inject Anchored context (identity, recent decisions,
relevant memories) with no manual action needed.
