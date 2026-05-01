# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.2.0] - 2025-04-30

### Added

- **Vector cache** (T1): in-memory RAM cache of all embedding vectors for sub-millisecond similarity search
- **PreTrainedTokenizerFast** (T3): full HuggingFace `tokenizer.json` support with BPE/WordPiece, normalizer pipeline, and automatic fallback to WordPiece (`vocab.txt`)
- **Model swap** (T4): switch from `all-MiniLM-L6-v2` to `paraphrase-multilingual-MiniLM-L12-v2` with automatic cache migration
- **Embedding cache migration** (T5): lazy re-embedding when model changes, old cache entries auto-invalidated
- **OpenCode importer** (T6): SQL-based import from `opencode.db` (sessions, messages, parts, todos)
- **Cursor rules importer** (T7): `.mdc` file parsing with YAML frontmatter (description, globs)
- **Incremental import tracking** (T8): `imports` table with delta sync per source (mtime, SHA-256, timestamps)
- **Entity detector** (T9): regex-based entity extraction from queries using project/keyword/content snapshots, with cached TTL
- **Topic change detector** (T10): detects conversation topic shifts to trigger broader, more diverse retrieval
- **Essential stories layer** (T11, L1): deterministic per-project summary template (top facts, decisions, events, preferences) with 6h SQLite cache
- **On-demand layer** (T12, L2): entity-driven FTS5 retrieval with category diversification and budget enforcement
- **Stack telemetry** (T21): atomic counters for L0/L1/L2 byte counts, L1 cache hit/miss stats
- **Memory indexer** (T16): heading-aware markdown chunking with SHA-256 delta sync and polling-based file watching
- **KG extractor** (T17): automatic pattern-based entity and relationship extraction on every save, rate-limited to 5 triples
- **Credential redaction** (T18): expanded regex patterns for API keys, tokens, passwords, SSH keys, and generic secrets
- **CLI refactoring** (T19): single `main.go` split into 11 separate command files with shared helpers
- **Config management** (T20): `anchored config show|set` with dotted key support
- **CHANGELOG.md** (T22): this file

### Changed

- Embedding model upgraded from `all-MiniLM-L6-v2` (English-only, 512 tokens) to `paraphrase-multilingual-MiniLM-L12-v2` (50+ languages, 128 tokens, 384 dims)
- Hybrid search now includes entity boost (1.1x) and project boost
- Memory stack L1 replaced generic project layer with deterministic essential stories
- Memory stack L2 now uses entity detection + topic change detection instead of simple regex lookup
- Import pipeline now uses `paraphrase-multilingual-MiniLM-L12-v2` instead of `bge-small-en-v1.5`
- Claude Code importer rewritten with robust multi-line JSON parsing and session summaries
- CLI split from single `main.go` (332 lines) into 11 files (~50-130 lines each)

### Fixed

- Download timeout increased from 5min to 10min for ~470MB model downloads
- HTTP resume support for interrupted model downloads (Range header)
- Duplicate map key compile errors in query expansion stop words
