# Architecture

## Overview

```
Claude Code --|
Cursor      --+--  MCP (JSON-RPC over STDIO)  -->  anchored serve --stdio  -->  ~/.anchored/data/anchored.db
OpenCode    --|
```

Single Go binary. Each AI tool spawns it on demand via STDIO. No daemon. No ports. SQLite WAL handles concurrent access from multiple tools.

## Directory Layout

```
anchored/
├── cmd/anchored/           CLI entry point
│   ├── main.go             Version, arg routing
│   ├── helpers.go          Shared types (serviceStoreAdapter, dbAccessor, initService)
│   ├── serve.go            MCP server (STDIO + SSE)
│   ├── init_cmd.go         Auto-detect tools + register MCP
│   ├── import_cmd.go       Multi-source import
│   ├── search.go           Terminal search
│   ├── save.go             Terminal save
│   ├── list.go             Terminal list
│   ├── forget.go           Terminal forget
│   ├── stats.go            Memory statistics (includes stack metrics)
│   ├── identity.go         View/edit ~/.anchored/identity.md
│   ├── config_cmd.go       Config show/set with dotted keys
│   └── main_test.go
│
├── pkg/
│   ├── mcp/                MCP JSON-RPC protocol
│   │   ├── server.go       STDIO/SSE transport
│   │   ├── tools.go        Tool definitions + handlers
│   │   ├── resources.go    MCP resources
│   │   └── protocol.go     Request/Response types
│   │
│   ├── memory/             Core storage and search
│   │   ├── store.go        Store interface
│   │   ├── sqlite_store.go SQLite: FTS5 + vector cache + hybrid search
│   │   ├── sqlite_schema.go
│   │   ├── sqlite_migrations.go
│   │   ├── service.go      Service layer (embedder, search, sanitizer, KG)
│   │   ├── embeddings.go   Provider interface (ONNX only)
│   │   ├── embeddings_onnx.go       Local ONNX Runtime
│   │   ├── embeddings_onnx_extract.go
│   │   ├── embedding_cache.go       Model-aware cache with migration
│   │   ├── vector_cache.go          In-memory RAM cache for fast similarity search
│   │   ├── quantized.go             uint8 quantization (4x reduction)
│   │   ├── hybrid_search.go         Vector + BM25 fusion with entity boost
│   │   ├── query_expansion.go       Stop words EN/PT/ES/FR, FTS5 query builder
│   │   ├── entity_detector.go       Regex-based entity extraction from queries
│   │   ├── topic_change_detector.go Detects conversation topic shifts
│   │   ├── tokenizer_fast.go        PreTrainedTokenizerFast (tokenizer.json)
│   │   ├── tokenizer.go             Tokenizer interface
│   │   ├── indexer.go               Heading-aware markdown chunking + SHA-256 delta sync
│   │   ├── import_tracker.go        Import tracking (delta sync metadata)
│   │   ├── categorizer.go           Auto-categorization (PT/EN)
│   │   ├── sanitizer.go             Secret/token redaction before store
│   │   └── text_util.go
│   │
│   ├── stack/              Memory stack (L0-L2)
│   │   ├── stack.go                Compositor with byte-budget enforcement + metrics
│   │   ├── layer_identity.go       L0: ~/.anchored/identity.md
│   │   ├── layer_essential.go      L1: per-project essential summary (6h cache)
│   │   ├── layer_ondemand.go       L2: entity-driven FTS5 retrieval with topic change
│   │   └── stack_test.go
│   │   layer_essential_test.go
│   │
│   ├── project/            Project detection and taxonomy
│   │   ├── detector.go     CWD → project resolution (git root, path mapping)
│   │   ├── taxonomy.go     Project/topic/namespace hierarchy
│   │   └── store.go        Project CRUD
│   │
│   ├── kg/                 Knowledge graph
│   │   ├── kg.go           Core: EnsureEntity, AddTriple, InvalidateTriple
│   │   ├── schema.go       DDL: entities, aliases, predicates, triples (bitemporal)
│   │   ├── extractor.go    Pattern-based triple extraction (regex, rate-limited)
│   │   ├── timeline.go     Temporal queries
│   │   ├── aliases.go      Entity alias resolution
│   │   └── privacy.go      Privacy controls
│   │
│   ├── importer/           Multi-source import
│   │   ├── importer.go     Interface + registry
│   │   ├── claude_code.go  Parse ~/.claude/projects/*.jsonl (robust multi-line JSON)
│   │   ├── opencode.go     Parse opencode.db (SQL queries, read-only)
│   │   ├── cursor_rules.go Parse .cursor/rules/*.mdc (frontmatter)
│   │   ├── devclaw.go      Parse data/memory.db
│   │   ├── memory_md.go    Parse MEMORY.md / CLAUDE.md files
│   │   └── directory.go    Index arbitrary directories
│   │
│   ├── setup/              Auto-detection and tool registration
│   │   ├── detector.go     Detect installed tools (CC, Cursor, OpenCode)
│   │   ├── registrar.go    Write MCP config for each tool
│   │   ├── config_writer.go
│   │   └── onnx_setup.go   Download ONNX Runtime + model
│   │
│   └── config/
│       └── config.go       YAML config struct (includes IndexerConfig)
│
├── install/
│   └── install.sh          curl | bash installer
├── configs/
│   └── config.example.yaml
├── docs/
│   ├── design.md
│   ├── architecture.md
│   ├── embedding-model.md
│   └── import-sources.md
├── go.mod
├── go.sum
├── Makefile
├── LICENSE
├── CHANGELOG.md
└── README.md
```

## Memory Stack

Three layers compose the context that gets injected into the agent's prompt:

```
+----------------------------------------------+
|          MemoryStack.Build()                  |
|          Budget: 3600 bytes (~900 tokens)     |
|                                               |
|  L0  Identity   --  ~/.anchored/identity.md   |
|                     Never trimmed.            |
|                     Hot-reloaded (fsnotify).  |
|                                               |
|  L1  Essential  --  Per-project summary       |
|                     SQLite cache, 6h TTL.     |
|                     Template-only, zero LLM.  |
|                     Top facts + decisions +   |
|                     events + preferences.     |
|                     Trimmed second.           |
|                                               |
|  L2  On-demand --  Entity-driven retrieval    |
|                     FTS5 search on detected   |
|                     entities. Topic change    |
|                     detection increases       |
|                     diversity.                |
|                     Trimmed first.            |
+----------------------------------------------+
```

- **L0** is loaded from `~/.anchored/identity.md`, hot-reloaded via fsnotify.
- **L1** is a deterministic markdown summary of the top facts, decisions, events, and preferences per project, cached in SQLite with 6h TTL.
- **L2** runs entity detection and topic change detection on the user's message, queries FTS5 for matches, and diversifies across categories.

When all layers render empty (no identity, no project detected, no entities), the stack returns empty string with zero overhead.

Stack metrics (L0/L1/L2 byte counts, L1 cache hits/misses) are tracked via atomic counters and exposed through `anchored stats`.

## Hybrid Search

```
Query
  +-- Entity Detection (regex snapshot from projects + memory keywords)
  |     +-- Entity boost x1.1 on matching memories
  |
  +-- Vector Search (cosine similarity, in-memory vector cache)
  |     +-- Quantized uint8 embeddings (asymmetric estimator)
  |
  +-- BM25 Search (SQLite FTS5, porter + unicode61 tokenizer)
  |     +-- Phrase query -> expanded keyword OR -> LIKE fallback
  |
  +-- Weighted Inverse Rank Fusion
        score = 0.7 x 1/(rank_vec + 1) + 0.3 x 1/(rank_bm25 + 1)
```

Post-processing:
- **Temporal decay** — exponential decay by memory age (configurable half-life)
- **MMR diversification** — Maximal Marginal Relevance re-ranking (lambda=0.7)
- **Project boost** — memories from the current project get score boost
- **Topic change** — increased limit + category diversification when topic shifts

## Embedding Pipeline

```
Text input
    |
    v
 PreTrainedTokenizerFast (tokenizer.json, 128 max tokens)
     |   Fallback: WordPiece (vocab.txt) for legacy model
     |
     v
 ONNX Runtime inference (paraphrase-multilingual-MiniLM-L12-v2)
     |   Graph optimizations: O1
     |   Threading: intra-op parallel
     |
     v
 Float32 vector (384 dims)
     |
     v
 Quantization -> uint8 (4x reduction, >=0.98 cosine correlation)
     |
     v
 Store in SQLite (BLOB) + in-memory vector cache
```

Model: `paraphrase-multilingual-MiniLM-L12-v2`, 384 dimensions, 128 max tokens, multilingual (50+ languages).

Cache migration: upgrading from the legacy `all-MiniLM-L6-v2` model automatically invalidates old cached embeddings. Re-embedding happens lazily.

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

Automatic extraction runs on every memory save via pattern-based regex (no LLM). Rate-limited to 5 triples per extraction to avoid noise. Text shorter than 20 chars is skipped.

`is_functional` predicates auto-invalidate old triples when a new one is added (e.g., "service X runs on port Y", old port is superseded).

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
| Model | paraphrase-multilingual-MiniLM-L12-v2 | Multilingual (50+ langs), PT+EN retrieval parity, 384 dims |
| Tokenizer | PreTrainedTokenizerFast | Full HuggingFace compat, WordPiece fallback |
| Quantization | uint8 | 4x memory reduction, >=0.98 correlation |
| Vector cache | In-memory RAM map | Sub-millisecond similarity lookup |
| Storage | SQLite WAL | Concurrent, zero-config, battle-tested in devclaw |
| Transport | MCP STDIO | Universal protocol, no daemon needed |
| Entity detection | Regex snapshot | No LLM, fast, project-aware |
| KG extraction | Pattern-based regex | No LLM needed, rate-limited, automatic |
| Sanitization | Regex pre-store | Never leak secrets into memory |
| CLI | Separate command files | Each command in its own file, shared helpers |
