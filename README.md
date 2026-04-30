# Anchored

> Persistent cross-tool memory for AI coding agents. Single binary. Zero dependencies.

[![License: MIT](https://img.shields.io/badge/license-MIT-green?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8?style=for-the-badge&logo=go)](https://go.dev)

Anchored is an MCP memory server that gives Claude Code, Cursor, OpenCode, and any MCP-compatible tool a shared, persistent memory. Install once — all your tools read, write, and search the same knowledge base.

No API keys. No daemon. All embeddings run locally.

---

## Install

```bash
curl -fsSL https://github.com/jholhewres/anchored/install.sh | bash
```

From source (Go 1.24+):
```bash
git clone https://github.com/jholhewres/anchored.git
cd anchored && make build && ./bin/anchored init
```

## Setup

```bash
anchored init          # auto-detects Claude Code, Cursor, OpenCode and registers MCP
anchored import --all  # imports existing sessions and memories
```

That's it. Open any tool — Anchored is available.

## Usage

### From your AI tool (via MCP)

Anchored registers as MCP tools automatically. Your agent can:

- `anchored_context` — load relevant memory for the current project
- `anchored_search` — search across all memories (semantic + keyword)
- `anchored_save` — persist a fact, decision, or preference
- `anchored_list` — list memories by category or project
- `anchored_forget` — remove outdated memories
- `kg_query` / `kg_add` — query and build a knowledge graph

### From the terminal

```bash
anchored search "deploy gateway"       # search memories
anchored save "API runs on :8080"      # save a fact
anchored list --category decision      # list decisions
anchored context                      # show context for current directory
anchored stats                        # memory overview
anchored import claude-code           # import from a specific source
anchored identity edit                 # edit your preferences
```

## What it imports

| Source | Extracts |
|---|---|
| Claude Code | JSONL sessions, `memory/` files, `CLAUDE.md` |
| OpenCode | `opencode.db` sessions, messages, todos |
| Cursor | `.cursor/rules/` |
| DevClaw | `data/memory.db` full memory + knowledge graph |
| Any directory | Markdown, code, docs |

All content is sanitized (secrets removed) before storage.

## What you end up with

```
~/.anchored/
├── bin/anchored           # the binary
├── data/
│   ├── anchored.db        # SQLite (FTS5 + vectors + knowledge graph)
│   └── onnx/              # local embedding model (~33MB)
└── config.yaml
```

No daemon. No ports. The binary runs on demand via MCP STDIO — your tool manages the lifecycle.

## Docs

- [Design](docs/design.md) — memory stack, hybrid search, knowledge graph, quantization
- [Architecture](docs/architecture.md) — project structure and implementation details
- [Import Sources](docs/import-sources.md) — how each tool's data is parsed

## License

[MIT](LICENSE)
