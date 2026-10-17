# What it does, in detail

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

## Features

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

## How it works

1. **Project detection** maps a working directory to a stable project record. Git remotes derive a cross-machine `remote_key` without leaking local paths.
2. **Memory save** sanitizes content, auto-categorizes when needed, attaches lifecycle metadata, stores SQLite/FTS rows, embeds asynchronously, and extracts KG triples when possible.
3. **Search** combines BM25, vector similarity, query expansion, entity boost, project boost, topic diversity, and lifecycle scoring.
4. **Context** renders a bounded XML-like bundle with identity, project stats, recent durable memories, and important recent session events.
5. **Curation** incrementally improves metadata quality so bad memories are demoted and sync filters can block them.
6. **Dream/retention** provide explicit deeper cleanup and lifecycle archiving.
7. **Remote sync** replicates project/team-safe semantic memories and KG triples when configured; local remains complete without remote.

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

## Configuration

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
