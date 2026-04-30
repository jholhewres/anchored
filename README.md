# Anchored

> Persistent cross-tool memory for AI coding agents. Single binary. Zero dependencies.

[![License: MIT](https://img.shields.io/badge/license-MIT-green?style=for-the-badge)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8?style=for-the-badge&logo=go)]
[![Release](https://img.shields.io/github/v/release/jholhewres/anchored?style=for-the-badge)](https://github.com/jholhewres/anchored/releases)

Anchored is an MCP memory server that gives Claude Code, Cursor, OpenCode, and any MCP-compatible tool a shared, persistent memory. Install once — all your tools read, write, and search the same knowledge base.

No API keys. No daemon. All embeddings run locally.

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

First run auto-downloads the embedding model (~33MB) and creates `~/.anchored/`.

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

## Tools

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

- **Hybrid search** — RRF fusion of vector similarity (ONNX, multilingual) and BM25 (FTS5)
- **Memory stack** — L0 identity + L1 project context + L2 on-demand, budget-enforced
- **Knowledge graph** — bitemporal triples with functional predicates and alias resolution
- **Content sanitization** — regex-based secret redaction before storage

## Storage

```
~/.anchored/
├── data/
│   ├── anchored.db        # SQLite (FTS5 + embedding cache + knowledge graph)
│   └── onnx/              # local embedding model (~33MB)
└── config.yaml
```

No daemon. No ports. The binary runs on demand via MCP STDIO.

## Docs

- [Design](docs/design.md) — memory stack, hybrid search, knowledge graph, quantization
- [Architecture](docs/architecture.md) — project structure and implementation details
- [Import Sources](docs/import-sources.md) — how each tool's data is parsed

## License

[MIT](LICENSE)
