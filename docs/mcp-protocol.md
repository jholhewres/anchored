# MCP Protocol

## Transport Modes

### STDIO (default)

The AI tool spawns `anchored serve --stdio` as a subprocess. Communication via stdin/stdout using JSON-RPC 2.0.

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.119"}}}
```

No daemon. No port. The tool manages the process lifecycle.

### SSE (optional)

```bash
anchored serve --port 4567
```

HTTP+SSE transport for programmatic access or multi-machine setups.

## Tool Definitions

### anchored_context

```json
{
  "name": "anchored_context",
  "description": "Load persistent memory context for the current working directory. ALWAYS call at conversation start. Returns your identity preferences, project summary, and relevant recent entities from ALL your coding tools (Claude Code, Cursor, OpenCode).",
  "inputSchema": {
    "type": "object",
    "properties": {
      "cwd": {
        "type": "string",
        "description": "Current working directory (auto-detected if omitted)"
      }
    }
  }
}
```

### anchored_search

```json
{
  "name": "anchored_search",
  "description": "Search your persistent cross-tool memory using hybrid semantic + keyword search. Use natural language queries. Returns ranked results with relevance scores.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "query": { "type": "string", "description": "Natural language search query" },
      "category": { "type": "string", "enum": ["fact","preference","decision","event","learning","plan"], "description": "Filter by category" },
      "project": { "type": "string", "description": "Filter by project name or path" },
      "limit": { "type": "integer", "default": 20, "description": "Max results" }
    },
    "required": ["query"]
  }
}
```

### anchored_save

```json
{
  "name": "anchored_save",
  "description": "Save a memory permanently. Auto-detects current project from CWD. Content is sanitized (secrets/tokens/passwords removed automatically). NEVER save API keys, tokens, passwords, or credentials.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "content": { "type": "string", "description": "Memory content to store" },
      "category": { "type": "string", "enum": ["fact","preference","decision","event","learning","plan"], "default": "fact" },
      "keywords": { "type": "array", "items": {"type": "string"}, "description": "Tags for better retrieval" },
      "project": { "type": "string", "description": "Project name (auto-detected from CWD)" }
    },
    "required": ["content"]
  }
}
```

### anchored_list

```json
{
  "name": "anchored_list",
  "description": "List memories filtered by category, project, or time range.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "category": { "type": "string", "enum": ["fact","preference","decision","event","learning","plan"] },
      "project": { "type": "string" },
      "limit": { "type": "integer", "default": 20 },
      "offset": { "type": "integer", "default": 0 }
    }
  }
}
```

### anchored_forget

```json
{
  "name": "anchored_forget",
  "description": "Remove a specific memory by ID. Use when information is outdated or incorrect.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "id": { "type": "string", "description": "Memory ID to remove" }
    },
    "required": ["id"]
  }
}
```

### anchored_stats

```json
{
  "name": "anchored_stats",
  "description": "Show memory statistics: total count, per-project breakdown, import coverage, embedding status.",
  "inputSchema": {
    "type": "object",
    "properties": {}
  }
}
```

### kg_query

```json
{
  "name": "kg_query",
  "description": "Query the knowledge graph for entities and their relationships. Find services, deployments, people, APIs, and their connections.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "query": { "type": "string", "description": "Entity name or relationship query" },
      "project": { "type": "string", "description": "Scope to project" },
      "include_triples": { "type": "boolean", "default": true }
    },
    "required": ["query"]
  }
}
```

### kg_add

```json
{
  "name": "kg_add",
  "description": "Add an entity or relationship to the knowledge graph. Use for structured knowledge like project dependencies, deployment targets, team members.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "action": { "type": "string", "enum": ["entity","triple"], "description": "What to add" },
      "name": { "type": "string", "description": "Entity name (for entity action)" },
      "subject": { "type": "string", "description": "Subject entity (for triple action)" },
      "predicate": { "type": "string", "description": "Relationship type (for triple action)" },
      "object": { "type": "string", "description": "Object entity (for triple action)" },
      "project": { "type": "string", "description": "Scope to project" }
    },
    "required": ["action"]
  }
}
```

## Resource Definitions

```json
{
  "uri": "anchored:///identity",
  "name": "Identity",
  "description": "User identity and preferences",
  "mimeType": "text/markdown"
}

{
  "uri": "anchored:///project/{path}",
  "name": "Project Context",
  "description": "Essential story for a specific project",
  "mimeType": "text/markdown"
}
```

Resources are read-only and used by agents to load structured context without consuming tool call budget.
