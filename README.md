# Anchored

> Persistent cross-tool memory for AI coding agents. Local-first, single binary, zero dependencies.

[![License: MIT](https://img.shields.io/badge/license-MIT-green?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25+-00ADD8?style=for-the-badge&logo=go)]
[![Release](https://img.shields.io/github/v/release/jholhewres/anchored?style=for-the-badge)](https://github.com/jholhewres/anchored/releases)

Anchored gives Claude Code, Cursor, OpenCode, Gemini CLI, Codex, VS Code Copilot, OpenClaw, Hermes, devclaw, gatorclaw, and other MCP-compatible tools one shared memory database on your machine.

- Local-first: no account, no cloud dependency, no required API key.
- One binary: `anchored` is both the CLI and MCP server.
- Fast retrieval: SQLite FTS5 + local ONNX embeddings + knowledge graph.
- Safe lifecycle: memories are scored, curated, inspected, exported, forgotten, and synced with privacy filters.

For team-shared project memory, the optional self-hosted/server side lives in [`anchored_oss`](../anchored_oss). Local Anchored remains the source of truth and the hot retrieval path.

## What Anchored remembers

Anchored stores small durable memories, not raw chat dumps by default. The public categories stay intentionally simple:

| Category | Use for |
|---|---|
| `fact` | Stable truths about a user, project, team, stack, API, or system. |
| `preference` | Personal preferences, project conventions, or team rules. Preferences have `user`/`project`/`team` scope. |
| `decision` | Architecture, tooling, naming, process, or product decisions. |
| `event` | Time-bound events: deploys, meetings, incidents, releases. |
| `learning` | Non-obvious lessons, gotchas, root causes, post-mortems. |
| `plan` | Intent, backlog, next steps, TODOs. |
| `summary` | Handoffs, recaps, precompact snapshots, daily/project summaries. |

Behind those categories, Anchored stores lifecycle metadata (`memory_type`, `kind`, `scope`, `origin`, `importance`, `confidence`, `expires_at`, `context_tier`, `curation_status`) so search, context injection, retention, curation, and remote sync make consistent decisions without making the MCP API harder for agents to use.

## Core features

- **Cross-tool memory** — all supported agents read/write the same local memory store.
- **Hybrid search** — RRF fusion of BM25, local multilingual embeddings, entity detection, project boost, topic diversity, and lifecycle scoring.
- **Knowledge graph** — pattern-based and explicit subject/predicate/object relationships, scoped by project.
- **Memory stack** — `anchored_context` returns identity, project stats, recent durable knowledge, and recent important session events under a tight budget.
- **Curated lifecycle** — default-on background curation scores recent memories in small batches and marks low-signal entries without deleting anything.
- **Dream review** — explicit/manual duplicate and contradiction analysis. Dedup can soft-delete; contradictions require manual review.
- **Privacy-first sync** — remote preview/sync block local paths, secrets, personal preferences, episodic/operational data, and low-quality memories by default.
- **Sandbox and indexing tools** — run code, process files, fetch docs, and index large output without flooding the model context.
- **Token telemetry** — every context injection records tokens injected vs. the static-context baseline it replaces; see it with `anchored stats --tokens` or the dashboard's Tokens card.
- **Broad host support** — one command registers Anchored into Claude Code, Cursor, OpenCode, Gemini, Windsurf, Cline, VS Code, Codex, Devin, OpenClaw, Hermes, devclaw, gatorclaw, with deep memory plugins for hosts that support one.
- **Local dashboard** — `anchored dashboard` serves a local UI: memories, knowledge graph, tasks, token savings, and a Connections view showing which hosts Anchored is wired into.
- **Inspection and export** — inspect exact metadata, list memories, export JSON/JSONL, restore curation backups, and purge safely.
- **Multi-source import** — Claude Code JSONL, OpenCode SQLite, Cursor `.mdc`, and DevClaw.

## Install

From GitHub Releases:

```bash
curl -fsSL https://raw.githubusercontent.com/jholhewres/anchored/main/install/install.sh | bash
```

From source:

```bash
git clone https://github.com/jholhewres/anchored.git
cd anchored
make build
sudo cp bin/anchored /usr/local/bin/
```

First run creates `~/.anchored/` and downloads the local embedding model when needed (~470 MB).

## Setup

### Claude Code plugin

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

### MCP only

```bash
claude mcp add -s user anchored anchored
```

The `-s user` flag makes Anchored available in every project. Without it, Claude Code registers the server only for the current project.

### Other tools

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

### VS Code Copilot

> **Note:** Hooks are in Preview in VS Code. See the
> [official documentation](https://code.visualstudio.com/docs/agent-customization/hooks)
> for details.

#### 1. MCP server

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

#### 2. Hooks (automatic context injection)

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

## CLI overview

```text
anchored                         Start MCP server over STDIO
anchored serve                   Start MCP server over STDIO
anchored init [--tool]           Register Anchored with supported tools
anchored doctor [--cwd]          Diagnose binary, model, DB, and MCP registration
anchored stats                   Show memory counts and import status
anchored stats --tokens          Show context tokens injected vs. baseline (7d)
anchored dashboard [--addr]      Serve the local dashboard UI

anchored save <content>          Save a memory
anchored search <query>          Search memories
anchored list                    List memories
anchored inspect <id>            Show full JSON metadata
anchored update <id>             Revise a memory in place
anchored forget <id>             Soft-delete a memory; --hard for permanent delete
anchored export                  Export memories as JSON/JSONL

anchored curation status         Show background curation worker state
anchored curation enable         Enable serve-time curation worker
anchored curation disable        Disable serve-time curation worker
anchored curation score          Score and optionally mark low-signal memories
anchored curation clean          Soft-delete or hard-delete low-signal memories
anchored curation restore        Restore a DB backup made before curation cleanup

anchored dream                   Analyze duplicate/contradictory memories
anchored dream --apply <id>      Apply one proposed dream action
anchored retention sweep         Archive expired operational/episodic memories
anchored bootstrap [--cwd]       Extract project seed memories from README/docs/rules/tree
anchored backfill [--batch]      Embed memories missing a vector (after bootstrap/import)
anchored handoff [--scope]       Save a short session handoff with TTL
anchored precompact              Save a pre-compaction recovery snapshot
anchored hook <subcommand>       Run session continuity hooks

anchored remote status           Show remote sync configuration
anchored remote configure        Configure a remote server
anchored remote link|unlink      Link/unlink remote project IDs
anchored remote preview          Offline preview of syncable/blocked memories
anchored remote sync             Push syncable memories and KG triples
anchored purge                   Wipe memories; --hard resets DB with backup
```

Import sources: `claude-code`, `devclaw`, `opencode`, `cursor`, `all`.

After `bootstrap` or `import`, run `anchored backfill` if memories still need vector embeddings for hybrid search. It is idempotent, a fully embedded store finishes immediately. Use `--pause 500ms` to stay gentle on CPU; `--skip-embeddings` on bootstrap/import defers this step.

## Curation vs dream

Anchored has two maintenance paths because they solve different problems:

| Path | Default | What it does | Safety model |
|---|---:|---|---|
| `curation` | On | Scores recent memories in small batches, sets `importance`, and marks `low_signal`. | Non-destructive. No content rewrites or deletes. |
| `dream` | Manual | Finds duplicates, merge/supersede opportunities, and contradictions. | Proposed actions; destructive operations require explicit apply/review. |

The curation worker starts with `anchored serve`. By default it runs every 15 minutes, processes newest candidates first, and updates at most 50 memories per pass. Tune or disable it with:

```bash
anchored curation status
anchored curation disable
anchored curation enable
anchored config set curation.interval_minutes 5
anchored config set curation.max_updates_per_run 25
anchored config set curation.threshold 0.55
```

Use `anchored curation clean --dry-run` when you want to remove low-signal memories. Cleanup is never automatic.

## MCP tools

### Memory tools

| Tool | Purpose |
|---|---|
| `anchored_context` | First call in a conversation; loads identity, project snapshot, recent durable memory. |
| `anchored_search` | Hybrid memory search, project-aware and remote-capable. |
| `anchored_save` | Save fact/preference/decision/event/learning/plan/summary memories. |
| `anchored_update` | Revise an existing memory instead of duplicating it. |
| `anchored_forget` | Soft-delete or hard-delete a memory. |
| `anchored_list` | List memories by category/project/time. |
| `anchored_stats` | Show memory statistics. |
| `anchored_session_end` | Close a tracked session and optionally save a summary. |
| `anchored_kg_query` | Query entity relationships from the knowledge graph. |
| `anchored_kg_add` | Add a relationship such as `repo — uses → Postgres`. |

### Sandbox and indexing tools

| Tool | Purpose |
|---|---|
| `anchored_execute` | Run code in a sandboxed subprocess; stdout only enters context. |
| `anchored_execute_file` | Process one file with injected `FILE_PATH`/`FILE_CONTENT`. |
| `anchored_batch_execute` | Run multiple commands, index large output, and search it in one call. |
| `anchored_index` | Index local markdown/prose into the sandbox FTS corpus. |
| `anchored_ctx_search` | Search the indexed sandbox corpus. |
| `anchored_fetch_and_index` | Fetch URL → markdown → index, with cache and batch support. |

## How it works

1. **Project detection** maps a working directory to a stable project record. Git remotes derive a cross-machine `remote_key` without leaking local paths.
2. **Memory save** sanitizes content, auto-categorizes when needed, attaches lifecycle metadata, stores SQLite/FTS rows, embeds asynchronously, and extracts KG triples when possible.
3. **Search** combines BM25, vector similarity, query expansion, entity boost, project boost, topic diversity, and lifecycle scoring.
4. **Context** renders a bounded XML-like bundle with identity, project stats, recent durable memories, and important recent session events.
5. **Curation** incrementally improves metadata quality so bad memories are demoted and sync filters can block them.
6. **Dream/retention** provide explicit deeper cleanup and lifecycle archiving.
7. **Remote sync** replicates project/team-safe semantic memories and KG triples when configured; local remains complete without remote.

## Storage

```text
~/.anchored/
├── config.yaml
├── debug.log                  # optional, only when debug.enabled=true
└── data/
    ├── anchored.db            # SQLite: memories, FTS5, vector cache, KG, sessions
    ├── bkps/                  # backups made by purge/restore/curation flows
    └── onnx/                  # local embedding model and ONNX runtime
```

No ports are opened. There is no always-on system daemon; the MCP server runs while the client keeps the `anchored` process alive.

## Configuration examples

```yaml
curation:
  enabled: true
  interval_minutes: 15
  threshold: 0.55
  max_updates_per_run: 50

sanitizer:
  enabled: true
  patterns:
    - 'ACME_[A-Z0-9]{32}'

context_optimizer:
  enabled: false
```

Show or edit config:

```bash
anchored config show
anchored config set curation.enabled false
anchored config wizard
```

## Development

```bash
make build
make test
./bin/anchored serve
```

Before a release, bump `VERSION`, run `make sync-version`, update `CHANGELOG.md`, then tag `vX.Y.Z`.

## Docs

- [Design](docs/design.md) — memory stack, hybrid search, knowledge graph, quantization.
- [Architecture](docs/architecture.md) — project structure and implementation details.
- [Embedding Model](docs/embedding-model.md) — model choice, quantization, inference pipeline.
- [Import Sources](docs/import-sources.md) — supported importers and parsing behavior.
- [MCP Protocol](docs/mcp-protocol.md) — tool schemas and protocol reference.
- [Team Sync](docs/team-sync.md) — local + remote architecture for shared project memory.
- [Improvements Roadmap](docs/improvements-roadmap.md) — local-first roadmap before/alongside team sync.
- [Memory Lifecycle](docs/memory-lifecycle-roadmap.md) — lifecycle classification, scoring, retention, and curation.
- [Changelog](CHANGELOG.md) — version history.

## License

[MIT](LICENSE)
