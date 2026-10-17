# CLI and MCP tools

## CLI

```text
anchored                         Start MCP server over STDIO
anchored serve                   Start MCP server over STDIO
anchored init [--tool]           Register Anchored with supported tools
anchored doctor [--cwd]          Diagnose binary, model, DB, and MCP registration
anchored self-update             Update the BINARY from the latest release
anchored stats                   Show memory counts and import status
anchored stats --tokens          Show context tokens injected vs. baseline (7d)
anchored dashboard [--addr]      Serve the local dashboard UI

anchored save <content>          Save a memory
anchored search <query>          Search memories
anchored list                    List memories
anchored inspect <id>            Show full JSON metadata
anchored update <id>             Revise a MEMORY in place (not the binary — see self-update)
anchored forget <id>             Soft-delete a memory; --hard for permanent delete
anchored export                  Export memories as JSON/JSONL

anchored curation status         Show background curation worker state
anchored curation enable         Enable serve-time curation worker
anchored curation disable        Disable serve-time curation worker
anchored curation score          Score and optionally mark low-signal memories
anchored curation clean          Soft-delete or hard-delete low-signal memories
anchored curation restore        Restore a DB backup made before curation cleanup

anchored dream                   Analyze duplicate/contradictory memories
anchored dream --apply <id>      Apply one proposed dream action
anchored retention sweep         Archive expired operational/episodic memories
anchored bootstrap [--cwd]       Extract project seed memories from README/docs/rules/tree
anchored backfill [--batch]      Embed memories missing a vector (after bootstrap/import)
anchored handoff [--scope]       Save a short session handoff with TTL
anchored precompact              Save a pre-compaction recovery snapshot
anchored hook <subcommand>       Run session continuity hooks

anchored remote status           Show remote sync configuration
anchored remote configure        Configure a remote server
anchored remote link|unlink      Link/unlink remote project IDs
anchored remote preview          Offline preview of syncable/blocked memories
anchored remote sync             Push syncable memories and KG triples
anchored purge                   Wipe memories; --hard resets DB with backup
```

Import sources: `claude-code`, `devclaw`, `opencode`, `cursor`, `all`.

After `bootstrap` or `import`, run `anchored backfill` if memories still need vector embeddings for hybrid search. It is idempotent, a fully embedded store finishes immediately. Use `--pause 500ms` to stay gentle on CPU; `--skip-embeddings` on bootstrap/import defers this step.

## MCP tools

### Memory tools

| Tool | Purpose |
|---|---|
| `anchored_context` | First call in a conversation; loads identity, project snapshot, recent durable memory. |
| `anchored_search` | Hybrid memory search, project-aware and remote-capable. |
| `anchored_save` | Save fact/preference/decision/event/learning/plan/summary memories. |
| `anchored_update` | Revise an existing memory instead of duplicating it. |
| `anchored_forget` | Soft-delete or hard-delete a memory. |
| `anchored_list` | List memories by category/project/time. |
| `anchored_stats` | Show memory statistics. |
| `anchored_session_end` | Close a tracked session and optionally save a summary. |
| `anchored_kg_query` | Query entity relationships from the knowledge graph. |
| `anchored_kg_add` | Add a relationship such as `repo — uses → Postgres`. |

### Sandbox and indexing tools

| Tool | Purpose |
|---|---|
| `anchored_execute` | Run code in a sandboxed subprocess; stdout only enters context. |
| `anchored_execute_file` | Process one file with injected `FILE_PATH`/`FILE_CONTENT`. |
| `anchored_batch_execute` | Run multiple commands, index large output, and search it in one call. |
| `anchored_index` | Index local markdown/prose into the sandbox FTS corpus. |
| `anchored_ctx_search` | Search the indexed sandbox corpus. |
| `anchored_fetch_and_index` | Fetch URL → markdown → index, with cache and batch support. |

Tool schemas and protocol details: [mcp-protocol.md](mcp-protocol.md).
