# Anchored

> Persistent cross-tool memory for AI coding agents. Single binary. Zero dependencies.

[![License: MIT](https://img.shields.io/badge/license-MIT-green?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8?style=for-the-badge&logo=go)](https://go.dev)

Anchored is an MCP memory server that gives your AI coding tools a shared, persistent brain. Install once, and Claude Code, Cursor, OpenCode, and any MCP-compatible tool can read, write, and search the same knowledge base — with hybrid semantic search, local embeddings, and a knowledge graph.

**Not a plugin. Not a framework.** Anchored is a standalone memory layer that sits between your tools and your brain. No API keys required — all embeddings run locally via ONNX Runtime.

---

## The Problem

You use Claude Code in the morning, Cursor in the afternoon, OpenCode at night. Each one forgets everything from the others. Decisions made in one tool must be re-explained in the next. `MEMORY.md` files are too flat. Session data is locked in proprietary formats.

```
Claude Code ──→  210 JSONL sessions in ~/.claude/projects/  (buried)
Cursor       ──→  .cursor/rules/ and chat history          (isolated)
OpenCode     ──→  opencode.db SQLite                      (different schema)
DevClaw      ──→  data/memory.db                          (powerful but trapped)
```

**Anchored unifies all of this.**

---

## Quick Start

```bash
# Install
curl -fsSL https://github.com/jholhewres/anchored/install.sh | bash

# Setup (auto-detects installed tools)
anchored init

# Import existing memories
anchored import --all

# Done. Open any tool — Anchored is available.
```

`anchored init` detects Claude Code, Cursor, and OpenCode automatically, registers the MCP server in each tool's config, and offers to import your existing session history.

From source (Go 1.24+):
```bash
git clone https://github.com/jholhewres/anchored.git && cd anchored
make build && ./bin/anchored init
```

---

## How It Works

```
Claude Code ──┐
Cursor      ──┼──  MCP (JSON-RPC over STDIO)  ──→  Anchored  ──→  SQLite + ONNX
OpenCode    ──┘                                   (local)
```

1. You install a single Go binary
2. `anchored init` registers it as an MCP server in each tool
3. Each tool spawns `anchored serve --stdio` on demand — no daemon, no port management
4. All tools read and write the same `~/.anchored/data/anchored.db`
5. SQLite WAL mode handles concurrent access from multiple tools
6. All embeddings run locally via ONNX Runtime (all-MiniLM-L6-v2, 384 dims, ~23MB)

### MCP Tools

| Tool | What it does |
|---|---|
| `anchored_context` | Load identity + project summary + relevant entities at session start |
| `anchored_search` | Hybrid semantic + keyword search across all memories |
| `anchored_save` | Persist a fact, decision, preference, or event |
| `anchored_list` | List memories by category, project, or time range |
| `anchored_forget` | Remove outdated or incorrect memories |
| `anchored_stats` | Memory statistics and import coverage |
| `kg_query` | Query the knowledge graph for entities and relationships |
| `kg_add` | Add structured entities and relationships |

### Memory Stack

```
L0  Identity      —  ~/.anchored/identity.md (your persistent preferences)
L1  Project       —  Per-project essential summary (cached 6h, zero LLM calls)
L2  On-demand     —  Entity-driven retrieval per turn (regex, no API)
```

### Import Sources

| Source | What it extracts |
|---|---|
| Claude Code | JSONL sessions → decisions, commands, errors, architecture |
| Claude Code memory | MEMORY.md files → facts, preferences, events |
| OpenCode | opencode.db → sessions, messages, todos |
| Cursor | .cursor/rules/ → project rules |
| DevClaw | data/memory.db → full memory + knowledge graph |
| Any directory | Markdown, code, docs → indexed and searchable |

All imported content is sanitized (secrets, tokens, passwords removed) before storage.

---

## CLI

```bash
anchored serve --stdio              # MCP server (used by tools automatically)
anchored serve --port 4567          # MCP SSE server (optional)

anchored search "deploy gateway"    # Search memories from terminal
anchored save "API on :8080"        # Save a fact
anchored list --category decision   # List decisions
anchored context                    # Show context for current directory
anchored stats                      # Overview

anchored import claude-code         # Import from ~/.claude/projects/
anchored import opencode            # Import from opencode.db
anchored import --all               # Auto-detect and import everything
anchored identity edit              # Edit your identity preferences

anchored init --tool claude-code    # Register in specific tool
anchored init --tool cursor
anchored init --tool opencode
```

---

## Architecture

```
anchored/
├── cmd/anchored/       CLI commands (Cobra)
├── pkg/
│   ├── mcp/            MCP JSON-RPC protocol
│   ├── memory/         SQLite FTS5 + vector search + embeddings
│   ├── stack/          L0-L2 memory stack
│   ├── project/        Project detection and taxonomy
│   ├── kg/             Knowledge graph (bitemporal triples)
│   ├── importer/       Multi-source memory import
│   └── setup/          Auto-detection and tool registration
├── install/            Installer script
└── configs/            Example configs
```

### Key Design Decisions

- **Single binary** — Go, statically compiled, zero runtime dependencies
- **Local-only embeddings** — ONNX Runtime + all-MiniLM-L6-v2, no API keys
- **Quantized vectors** — uint8 embeddings (4x memory reduction, ≥0.98 correlation)
- **Hybrid search** — vector similarity + BM25 with weighted reciprocal rank fusion
- **No daemon required** — MCP STDIO mode, tools manage process lifecycle
- **Content sanitization** — secrets/tokens/passwords stripped before storage
- **SQLite WAL** — concurrent access from multiple tools without a server

---

## Storage

```
~/.anchored/
├── bin/anchored           # Binary
├── data/
│   ├── anchored.db        # SQLite (FTS5 + vectors + KG)
│   └── onnx/              # Local embedding model (~23MB)
└── config.yaml            # Configuration
```

---

## License

[MIT](LICENSE)
