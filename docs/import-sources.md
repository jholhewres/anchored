# Import Sources

Anchored imports memories from existing AI coding tools and stores them in a unified format with embeddings and knowledge graph entries.

## Supported Sources

### Claude Code

**Status:** Complete

**Location:** `~/.claude/projects/{path-hash}/`

Path hash = project path with `/` to `-` and leading `/` stripped.
Example: `/home/jhol/Workspace/private/devclaw` to `-home-jhol-Workspace-private-devclaw`

**What exists there:**
```
{path-hash}/
├── {uuid}.jsonl              # Session transcripts
├── {uuid}/                   # Sessions with subagents
│   ├── subagents/
│   │   ├── agent-{id}.jsonl  # Subagent conversations
│   │   └── agent-{id}.meta.json
│   └── tool-results/
├── memory/
│   ├── MEMORY.md             # Persistent memory with YAML frontmatter
│   └── *.md                  # Additional memory files
└── .mcp.json                 # Project-level MCP config
```

**JSONL format — each line is a JSON object:**
```json
{
  "type": "user",
  "uuid": "...",
  "sessionId": "4480f03a-...",
  "cwd": "/home/jhol/Workspace/private/devclaw",
  "timestamp": "2026-04-27T13:49:54.777Z",
  "version": "2.1.119",
  "gitBranch": "master",
  "message": {
    "role": "user",
    "content": "How does the deploy work?"
  }
}
```

**Assistant messages can have structured content blocks:**
```json
{
  "type": "assistant",
  "message": {
    "role": "assistant",
    "content": [
      {"type": "text", "text": "To deploy..."},
      {"type": "tool_use", "id": "...", "name": "Bash", "input": {"command": "..."}},
      {"type": "tool_result", "tool_use_id": "...", "content": "..."}
    ]
  }
}
```

**Import strategy:**
1. Scan all `{path-hash}/*.jsonl` files
2. Group lines by `sessionId` to form one session
3. Map `cwd` to project (git root detection)
4. Filter `type: "user"` and `type: "assistant"` messages with text content
5. Extract from each session:
   - **Summaries** — first assistant message per session used as summary
   - **Decisions** — patterns: "vamos", "decidimos", "implementar", "refatorar"
   - **Commands** — `tool_use` with `name: "Bash"` or `name: "Write"`
   - **Files modified** — `tool_use` with `name: "Edit"` or `name: "Write"`
   - **Errors found** — error patterns in tool_result content
6. Parse `memory/*.md` files (YAML frontmatter: name, description, type)
7. Parse `CLAUDE.md` files (project-level and global `~/.claude/CLAUDE.md`)

The parser handles multi-line JSON objects (JSONL where one logical entry spans multiple lines), robust error recovery, and mixed content types.

**MCP registration:** `~/.claude/mcp.json` (global) or `.mcp.json` (per-project)
```json
{
  "mcpServers": {
    "anchored": {
      "command": "/home/jhol/.anchored/bin/anchored",
      "args": ["serve", "--stdio"]
    }
  }
}
```

---

### OpenCode

**Status:** Complete

**Location:** `~/.local/share/opencode/opencode.db`
**Config:** `~/.local/share/opencode/opencode.json`

**Schema (Drizzle ORM):**
```sql
project       (id, worktree, vcs, name, time_created, time_updated)
session       (id, project_id, slug, directory, title, version, time_created, time_updated)
message       (id, session_id, data TEXT)         -- data = JSON: {"role": "user"} or {"role": "assistant"}
part          (id, message_id, session_id, data TEXT)  -- data = JSON: {"type": "text", "text": "..."}
todo          (session_id, content, status, priority, position, time_created, time_updated)
```

**Part data format:**
```json
{"type": "text", "text": "User message content..."}
{"type": "step-start"}
{"type": "reasoning", "text": "Agent thinking...", "time": {"start": 1700000000}}
{"type": "tool", "tool": "todowrite", "callID": "...", "state": {"status": "completed", "input": {...}}}
{"reason": "tool-calls", "type": "step-finish", "tokens": {"total": 32281, ...}}
```

**Import strategy:**
1. `JOIN session s ON part.session_id = s.id JOIN project p ON s.project_id = p.id`
2. Filter parts where `type = "text"` with non-empty `text`
3. Map `s.directory` to project (fallback to `project.worktree`)
4. Import todos as `plan` category memories
5. Auto-categorize text parts using regex patterns

Opens the database in read-only mode (`?_mode=ro`) for safety.

**MCP registration:** `~/.config/opencode/opencode.json`
```json
{
  "mcp": {
    "anchored": {
      "command": "/home/jhol/.anchored/bin/anchored",
      "args": ["serve", "--stdio"]
    }
  }
}
```

---

### Cursor

**Status:** Complete

**Location:** `~/.cursor/`

**What to import:**
- `~/.cursor/rules/*.mdc` — global rules (markdown with cursor frontmatter)
- `.cursor/rules/*.mdc` — per-project rules
- `.cursor/mcp.json` — existing MCP config (read-only, don't overwrite)

**.mdc format:**
```markdown
---
description: Architecture rules
globs: src/components/**
---
Rule content here...
```

**Import strategy:**
1. Parse YAML frontmatter (description, globs) using line-by-line splitting
2. Map to project based on file location
3. Store as `preference` category
4. Handle edge cases: no frontmatter (entire content is body), malformed frontmatter (skip)
5. JSON array values in globs parsed via `json.Unmarshal`

**MCP registration:** `~/.cursor/mcp.json`
```json
{
  "mcpServers": {
    "anchored": {
      "command": "/home/jhol/.anchored/bin/anchored",
      "args": ["serve", "--stdio"]
    }
  }
}
```

---

### DevClaw

**Status:** Stub (basic import available)

**Location:** `{project}/data/memory.db` + `{project}/data/memory/`

**Full import** — everything the devclaw memory system stores:
- SQLite memories table (with categories, keywords, embeddings, wing metadata)
- FileStore markdown files (fact/, preference/, event/, summary/)
- Knowledge graph (kg_entities, kg_triples)
- Essential stories cache

**Import strategy:**
1. Read devclaw SQLite directly (same schema)
2. Copy memories with source metadata set to `devclaw`
3. Copy KG entities and triples
4. Re-embed if model differs (devclaw might use different embedding provider)

---

### Generic Directory

**Any path** — indexes markdown, code, and text files.

**Import strategy:**
1. Walk directory recursively
2. Filter by extension (`.md`, `.go`, `.ts`, `.tsx`, `.py`, `.yaml`, `.json`)
3. Markdown chunking at heading/paragraph boundaries
4. SHA-256 delta sync (skip unchanged files)
5. Map directory to project (git root detection)

---

## Import Pipeline

All sources share the same post-processing pipeline:

```
Raw content from source
    |
    v
Content sanitizer --→ Remove API keys, tokens, passwords, SSH keys
    |
    v
Auto-categorizer --→ fact | preference | decision | event | learning | plan
    |                 (regex patterns, PT/EN)
    |
    v
Project resolver --→ CWD or file path → project (git root)
    |
    v
ONNX embedding --→ paraphrase-multilingual-MiniLM-L12-v2 → uint8 quantized vector
    |
    v
SQLite store --→ memories table + FTS5 index + vector cache
    |
    v
KG extractor --→ Pattern-based entity and relationship extraction (max 5 triples/save)
    |
    v
Done. Memory is searchable from all tools.
```

## Incremental Import

Imports are tracked in the `imports` table. Re-running `anchored import` only processes new/changed data:

```sql
CREATE TABLE imports (
    id, source, path,
    memories_imported, entities_imported,
    status,            -- pending | running | done | error
    started_at, finished_at, error
);
```

Delta sync varies by source:
- **Claude Code**: file modification time of JSONL files vs `finished_at`
- **OpenCode**: max `time_updated` in sessions vs `finished_at`
- **Cursor**: file modification time of .mdc files vs `finished_at`
- **DevClaw**: max row `created_at` in memories table vs `finished_at`
- **Directory**: SHA-256 hash comparison

Use `--force` to bypass delta sync and re-import everything.
