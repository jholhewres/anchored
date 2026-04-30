# Architecture

## Overview

```
Claude Code ──┐
Cursor      ──┼──  MCP (JSON-RPC over STDIO)  ──→  anchored serve --stdio  ──→  ~/.anchored/data/anchored.db
OpenCode    ──┘
```

Single Go binary. Each AI tool spawns it on demand via STDIO. No daemon. No ports. SQLite WAL handles concurrent access from multiple tools.

## Directory Layout

```
anchored/
├── cmd/anchored/           CLI entry point (Cobra)
│   ├── main.go
│   ├── serve.go            # MCP server (STDIO + SSE)
│   ├── init_cmd.go         # Auto-detect tools + register MCP
│   ├── import_cmd.go       # Multi-source import
│   ├── search_cmd.go       # Terminal search
│   ├── save_cmd.go         # Terminal save
│   ├── list_cmd.go
│   ├── forget_cmd.go
│   ├── context_cmd.go      # Show L0+L1+L2 for CWD
│   ├── stats_cmd.go
│   ├── identity_cmd.go     # Edit ~/.anchored/identity.md
│   ├── daemon_cmd.go       # Optional background consolidation
│   └── config_cmd.go
│
├── pkg/
│   ├── mcp/                MCP JSON-RPC protocol
│   │   ├── server.go       # STDIO/SSE transport
│   │   ├── tools.go        # Tool definitions + handlers
│   │   ├── resources.go    # MCP resources
│   │   └── protocol.go     # Request/Response types
│   │
│   ├── memory/             Core storage and search
│   │   ├── store.go        # Store interface
│   │   ├── sqlite_store.go # SQLite: FTS5 + vector cache + hybrid search
│   │   ├── sqlite_schema.go
│   │   ├── sqlite_migrations.go
│   │   ├── embeddings.go   # Provider interface (ONNX only)
│   │   ├── embeddings_onnx.go      # Local ONNX Runtime
│   │   ├── embeddings_onnx_extract.go
│   │   ├── quantized.go    # uint8 quantization (4x reduction)
│   │   ├── hybrid_search.go        # Vector + BM25 fusion
│   │   ├── query_expansion.go      # Stop words EN/PT/ES/FR, FTS5 builder
│   │   ├── indexer.go       # Markdown chunking + SHA-256 delta sync
│   │   ├── categorizer.go   # Auto-categorization (PT/EN)
│   │   ├── sanitizer.go     # Secret/token redaction before store
│   │   ├── entity_detector.go      # Regex-based entity extraction
│   │   ├── topic_change_detector.go
│   │   ├── tokenizer_wordpiece.go  # ONNX WordPiece tokenizer
│   │   └── text_util.go
│   │
│   ├── stack/              Memory stack (L0-L2)
│   │   ├── layer_identity.go       # L0: ~/.anchored/identity.md
│   │   ├── layer_project.go        # L1: per-project essential summary (6h cache)
│   │   ├── layer_ondemand.go       # L2: per-turn entity retrieval
│   │   └── stack.go                # Compositor with byte-budget enforcement
│   │
│   ├── project/            Project detection and taxonomy
│   │   ├── detector.go     # CWD → project resolution (git root, path mapping)
│   │   ├── taxonomy.go     # Project/topic/namespace hierarchy
│   │   └── store.go        # Project CRUD
│   │
│   ├── kg/                 Knowledge graph
│   │   ├── kg.go           # Core: EnsureEntity, AddTriple, InvalidateTriple
│   │   ├── schema.go       # DDL: entities, aliases, predicates, triples (bitemporal)
│   │   ├── extractor.go    # Pattern-based triple extraction
│   │   ├── timeline.go     # Temporal queries
│   │   ├── aliases.go      # Entity alias resolution
│   │   └── privacy.go      # Privacy controls
│   │
│   ├── importer/           Multi-source import
│   │   ├── importer.go     # Interface + registry
│   │   ├── claude_code.go  # Parse ~/.claude/projects/*.jsonl
│   │   ├── opencode.go     # Parse opencode.db
│   │   ├── cursor_rules.go # Parse .cursor/rules/
│   │   ├── devclaw.go      # Parse data/memory.db
│   │   ├── memory_md.go    # Parse MEMORY.md / CLAUDE.md files
│   │   └── directory.go    # Index arbitrary directories
│   │
│   ├── setup/              Auto-detection and tool registration
│   │   ├── detector.go     # Detect installed tools (CC, Cursor, OpenCode)
│   │   ├── registrar.go    # Write MCP config for each tool
│   │   ├── config_writer.go
│   │   └── onnx_setup.go   # Download ONNX Runtime + model
│   │
│   └── config/
│       └── config.go       # YAML config struct
│
├── install/
│   └── install.sh          # curl | bash installer
├── configs/
│   └── config.example.yaml
├── go.mod
├── go.sum
├── Makefile
├── LICENSE
└── README.md
```

## Memory Stack

Three layers compose the context that gets injected into the agent's prompt:

```
┌──────────────────────────────────────────┐
│          MemoryStack.Build()             │
│          Budget: 3600 bytes (~900 tokens)│
│                                          │
│  L0  Identity   ──  ~/.anchored/identity.md
│                     Never trimmed. User preferences.
│                                          │
│  L1  Project    ──  Per-project summary
│                     SQLite cache, 6h TTL. Template-only, zero LLM.
│                     Trimmed second when budget exceeded.
│                                          │
│  L2  On-demand  ──  Entity-driven retrieval
│                     Regex tokenization → SQL lookup per turn.
│                     Trimmed first when budget exceeded.
└──────────────────────────────────────────┘
```

- **L0** is loaded from `~/.anchored/identity.md`, hot-reloaded via fsnotify.
- **L1** is a deterministic markdown summary of the top files per project, cached in SQLite.
- **L2** runs regex entity detection on the user's message, queries the DB for matches.

When all layers render empty (no identity, no project detected, no entities), the stack returns empty string — zero overhead.

## Hybrid Search

```
Query
 ├── Vector Search (cosine similarity, in-memory vector cache)
 │   └── Quantized uint8 embeddings (asymmetric estimator)
 │
 ├── BM25 Search (SQLite FTS5, porter + unicode61 tokenizer)
 │   └── Phrase query → expanded keyword OR → LIKE fallback
 │
 └── Weighted Inverse Rank Fusion
     score = 0.7 × 1/(rank_vec + 1) + 0.3 × 1/(rank_bm25 + 1)
```

Post-processing:
- **Temporal decay** — exponential decay by memory age (configurable half-life)
- **MMR diversification** — Maximal Marginal Relevance re-ranking (lambda=0.7)
- **Project boost** — memories from the current project get score boost

## Embedding Pipeline

```
Text input
    │
    ▼
WordPiece tokenizer (vocab.txt, 512 max tokens)
    │
    ▼
ONNX Runtime inference (paraphrase-multilingual-MiniLM-L12-v2)
    │
    ▼
Float32 vector (384 dims)
    │
    ▼
Quantization → uint8 (4x reduction, ≥0.98 cosine correlation)
    │
    ▼
Store in SQLite (BLOB) + in-memory vector cache
```

## Knowledge Graph

Bitemporal SPO triples with provenance:

```
kg_entities          id, name, project_id, embedding
kg_entity_aliases    entity_id, alias
kg_predicates        id, name, is_functional
kg_triples           subject_id, predicate_id, object_id,
                     confidence, project_id,
                     valid_from, valid_to, txn_time
```

Pattern-based extraction runs automatically. Entities and relationships are extracted from memory text using regex patterns (dates, emails, URLs, deployments, etc.).

`is_functional` predicates auto-invalidate old triples when a new one is added (e.g., "service X runs on port Y" — old port is superseded).

## Content Sanitization

Before any memory is stored, it passes through regex-based sanitization:

- API keys (`sk-...`, `key-...`, `AKIA...`)
- Bearer tokens (`Bearer ...`)
- Passwords (`password`, `passwd`, `pwd` followed by `=`/`:`)
- Private keys (`-----BEGIN ... PRIVATE KEY-----`)
- SSH keys (`ssh-rsa`, `ssh-ed25519`)
- Generic secrets (`secret`, `credential`, `token` patterns)

Matched content is redacted to `[REDACTED]`.

## SQLite Concurrency

```sql
PRAGMA journal_mode = WAL;        -- concurrent reads + 1 writer
PRAGMA busy_timeout = 30000;       -- 30s wait for write lock
PRAGMA txlock = immediate;         -- writer gets lock immediately
```

Multiple tools can spawn `anchored serve --stdio` simultaneously. SQLite WAL serializes writes and allows unlimited concurrent reads.

## MCP Transport

**STDIO (default):**
- Tool spawns binary, communicates via stdin/stdout JSON-RPC
- No daemon, no port management
- Process dies when the tool closes

**SSE (optional):**
- `anchored serve --port 4567`
- For programmatic access or multi-machine setups

## Key Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Language | Go | Static binary, cross-compile, devclaw proven |
| Embeddings | ONNX local only | Privacy, zero API keys, instant |
| Model | paraphrase-multilingual-MiniLM-L12-v2 | Multilingual (50+ langs), PT+EN retrieval parity, same 384 dims |
| Quantization | uint8 | 4x memory reduction, ≥0.98 correlation |
| Storage | SQLite WAL | Concurrent, zero-config, battle-tested in devclaw |
| Transport | MCP STDIO | Universal protocol, no daemon needed |
| Sanitization | Regex pre-store | Never leak secrets into memory |
