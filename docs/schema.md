# SQLite Schema

## Core Tables

```sql
-- Persistent memories
CREATE TABLE memories (
    id TEXT PRIMARY KEY,
    project_id TEXT,                -- NULL = global/user-level
    category TEXT NOT NULL,         -- fact | preference | decision | event | learning | plan
    content TEXT NOT NULL,
    keywords TEXT,                  -- JSON array of tags
    embedding BLOB,                 -- Quantized uint8 vector (384 × 1 byte)
    source TEXT,                    -- claude-code | opencode | cursor | devclaw | manual
    source_id TEXT,                 -- Original session ID or file path
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    access_count INTEGER DEFAULT 0,
    last_accessed_at DATETIME,
    metadata TEXT                   -- JSON object with extra fields
);

-- Full-text search
CREATE VIRTUAL TABLE memories_fts USING fts5(
    content,
    keywords,
    content='memories',
    content_rowid='rowid',
    tokenize='porter unicode61'
);

-- Embedding cache (avoid re-computing)
CREATE TABLE embedding_cache (
    text_hash TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT 'bge-small-en-v1.5',
    embedding BLOB NOT NULL,         -- Quantized uint8
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (text_hash, model)
);

-- In-memory vector cache loaded at startup
-- Key: rowid, Value: float32 vector
-- Evicted by LRU when memory pressure
```

## Projects

```sql
CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    path TEXT UNIQUE NOT NULL,      -- Canonical path (realpath)
    source_tool TEXT,               -- claude-code | opencode | cursor | devclaw | manual
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

## Memory Stack

```sql
-- L1 essential story cache per project
CREATE TABLE essential_stories (
    project_id TEXT PRIMARY KEY REFERENCES projects(id),
    story TEXT,                     -- Markdown summary
    source_memories TEXT,           -- JSON array of memory IDs used
    generated_at DATETIME,
    bytes INTEGER,                  -- Story size in bytes
    schema_version INTEGER DEFAULT 1
);
```

## Knowledge Graph

```sql
CREATE TABLE kg_entities (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    project_id TEXT REFERENCES projects(id),
    embedding BLOB,                 -- Quantized uint8
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE kg_entity_aliases (
    entity_id TEXT NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    alias TEXT NOT NULL,
    PRIMARY KEY (entity_id, alias)
);

CREATE TABLE kg_predicates (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    is_functional BOOLEAN DEFAULT FALSE,  -- Auto-invalidate old triples
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE kg_triples (
    id TEXT PRIMARY KEY,
    subject_id TEXT NOT NULL REFERENCES kg_entities(id),
    predicate_id TEXT NOT NULL REFERENCES kg_predicates(id),
    object_id TEXT NOT NULL REFERENCES kg_entities(id),
    confidence REAL DEFAULT 1.0,
    project_id TEXT REFERENCES projects(id),
    -- Bitemporal tracking
    valid_from DATETIME DEFAULT CURRENT_TIMESTAMP,
    valid_to DATETIME,              -- NULL = still valid
    txn_time DATETIME DEFAULT CURRENT_TIMESTAMP,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_triples_subject ON kg_triples(subject_id);
CREATE INDEX idx_triples_predicate ON kg_triples(predicate_id);
CREATE INDEX idx_triples_object ON kg_triples(object_id);
CREATE INDEX idx_triples_project ON kg_triples(project_id);
CREATE INDEX idx_triples_valid ON kg_triples(valid_from, valid_to);
```

## Import Tracking

```sql
CREATE TABLE imports (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,            -- claude-code | opencode | cursor | devclaw | directory
    path TEXT NOT NULL,
    memories_imported INTEGER DEFAULT 0,
    entities_imported INTEGER DEFAULT 0,
    status TEXT DEFAULT 'pending',   -- pending | running | done | error
    started_at DATETIME,
    finished_at DATETIME,
    error TEXT,
    last_checksum TEXT               -- For delta detection
);
```

## Session Index

```sql
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    project_id TEXT REFERENCES projects(id),
    source TEXT NOT NULL,            -- claude-code | opencode | cursor | devclaw
    source_session_id TEXT,
    title TEXT,
    directory TEXT,
    created_at DATETIME,
    message_count INTEGER DEFAULT 0,
    memory_count INTEGER DEFAULT 0,  -- How many memories were extracted
    imported_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_sessions_project ON sessions(project_id);
CREATE INDEX idx_sessions_source ON sessions(source);
```

## PRAGMA Configuration

```sql
PRAGMA journal_mode = WAL;        -- Concurrent reads + serialized writes
PRAGMA busy_timeout = 30000;       -- Wait up to 30s for write lock
PRAGMA txlock = immediate;         -- Writer grabs lock immediately
PRAGMA cache_size = -64000;        -- 64MB page cache
PRAGMA synchronous = NORMAL;       -- Flush less often (safe with WAL)
PRAGMA foreign_keys = ON;          -- Enforce referential integrity
```

## Indexes

```sql
CREATE INDEX idx_memories_project ON memories(project_id);
CREATE INDEX idx_memories_category ON memories(category);
CREATE INDEX idx_memories_source ON memories(source);
CREATE INDEX idx_memories_created ON memories(created_at);
CREATE INDEX idx_memories_accessed ON memories(last_accessed_at);
```
