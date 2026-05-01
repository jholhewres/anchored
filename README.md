# Anchored

> Persistent cross-tool memory for AI coding agents. Single binary. Zero dependencies.

[![License: MIT](https://img.shields.io/badge/license-MIT-green?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8?style=for-the-badge&logo=go)]
[![Release](https://img.shields.io/github/v/release/jholhewres/anchored?style=for-the-badge)](https://github.com/jholhewres/anchored/releases)

Anchored is an MCP memory server that gives Claude Code, Cursor, OpenCode, and any MCP-compatible tool a shared, persistent memory. Install once, and all your tools read, write, and search the same knowledge base.

No API keys. No daemon. All embeddings run locally.

## Features

- **Multilingual embeddings** — `paraphrase-multilingual-MiniLM-L12-v2` (50+ languages, PT-BR and EN parity)
- **Hybrid search** — RRF fusion of vector similarity (384-dim ONNX) and BM25 (FTS5), with entity boost and project boost
- **Entity detection** — regex-based extraction of project names, tools, and topics from queries, boosting relevant results
- **Topic change detection** — identifies conversation shifts and increases retrieval diversity
- **Memory stack** — L0 identity + L1 essential stories + L2 on-demand retrieval, budget-enforced (~900 tokens)
- **Vector cache** — in-memory RAM cache of embeddings for fast similarity search
- **Incremental indexer** — polling-based with SHA-256 delta sync, heading-aware chunking
- **Knowledge graph** — automatic pattern-based extraction of entities and relationships (no LLM needed)
- **Credential redaction** — regex-based secret sanitization before storage
- **Multi-source import** — Claude Code (JSONL), OpenCode (SQLite), Cursor (.mdc rules), DevClaw

## Install

From [GitHub Releases](https://github.com/jholhewres/anchored/releases):

```bash
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/jholhewres/anchored/main/install/install.sh | bash
```

From source:

```bash
git clone https://github.com/jholhewres/anchored.git
cd anchored && make build
sudo cp bin/anchored /usr/local/bin/
```

First run auto-downloads the embedding model (~470MB) and creates `~/.anchored/`.

## Setup

Add Anchored as an MCP server to your tool:

**Claude Code** (`.claude/settings.json`):
```json
{
  "mcpServers": {
    "anchored": {
      "command": "anchored"
    }
  }
}
```

**Cursor** / **OpenCode** — add the same MCP config in your tool's settings.

## CLI

```
anchored                    Start MCP server (STDIO)
anchored serve              Start MCP server (STDIO)
anchored import [sources]   Import memories from detected sources
anchored search <query>     Search memories
anchored save <content>     Save a memory
anchored list               List memories
anchored forget <id>        Remove a memory
anchored stats              Show memory statistics
anchored identity [edit]    View or edit identity file
anchored config [show|set]  View or modify configuration
anchored init               Auto-detect tools and register MCP
```

Import sources: `claude-code` `devclaw` `opencode` `cursor` `all`

## MCP Tools

| Tool | Description |
|---|---|
| `anchored_context` | Load relevant memory for the current project |
| `anchored_search` | Search across all memories (semantic + keyword) |
| `anchored_save` | Persist a fact, decision, or preference |
| `anchored_list` | List memories by category or project |
| `anchored_forget` | Remove a memory |
| `anchored_stats` | Memory overview |
| `kg_query` | Query the knowledge graph |
| `kg_add` | Add a relationship to the knowledge graph |

## How it works

- **Hybrid search** — RRF fusion of vector similarity (ONNX, multilingual) and BM25 (FTS5), with entity boost and project boost
- **Entity detection** — extracts project names, tools, and topics from queries to boost relevant results
- **Topic change detection** — identifies conversation shifts and increases retrieval diversity
- **Memory stack** — L0 identity + L1 essential stories + L2 on-demand, budget-enforced
- **Knowledge graph** — bitemporal triples with functional predicates and alias resolution, auto-extracted from memory text
- **Credential redaction** — regex-based secret sanitization before storage

## Storage

```
~/.anchored/
├── data/
│   ├── anchored.db        # SQLite (FTS5 + vector cache + knowledge graph)
│   └── onnx/              # local embedding model (~470MB)
└── config.yaml
```

No daemon. No ports. The binary runs on demand via MCP STDIO.

## Docs

- [Design](docs/design.md) — memory stack, hybrid search, knowledge graph, quantization
- [Architecture](docs/architecture.md) — project structure and implementation details
- [Embedding Model](docs/embedding-model.md) — model choice, quantization, inference pipeline
- [Import Sources](docs/import-sources.md) — how each tool's data is parsed
- [Changelog](CHANGELOG.md) — version history

## License

[MIT](LICENSE)
