package mcp

import (
	"sort"
)

func ToolDefinitions() []Tool {
	return []Tool{
		{
			Name:        "anchored_context",
			Description: "First call of every new conversation. Returns identity, project summary, recent decisions, and durable memories from this user across every AI tool and IDE. Re-call when project (cwd) changes. Example: user says \"can you help with the auth flow?\" → call anchored_context(cwd=\".\") before reading code.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project detection",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "Optional session ID for tracking",
					},
				},
				"required": []string{"cwd"},
			},
		},
		{
			Name:        "anchored_search",
			Description: "Search persistent memory before answering. When anchored_context reports an active remote, procedural work must search and load anchored_skill first; use memory search after skill selection or when no skill matches. Use silently — never narrate \"let me check memory\". Triggers: past work, prior decisions, conventions, preferences, or a named project/service/library. Hybrid vector + BM25, milliseconds.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural language search query",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project-scoped search. When omitted, searches globally across all projects.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Filter by category: fact, preference, decision, event, learning, plan, summary",
						"enum":        []string{"fact", "preference", "decision", "event", "learning", "plan", "summary"},
					},
					"max_results": map[string]any{
						"type":        "integer",
						"description": "Maximum results to return (default: 20)",
					},
					"remote": map[string]any{
						"type":        "string",
						"description": "Usually OMIT this: when the project has a remote configured, remote results are merged into every search automatically. Set it only to search a remote EXCLUSIVELY. Empty string or \"default\" means THIS REPO'S remote (resolved by git origin, the same routing sync uses — not necessarily a server named default); any other value is a server name from the config. If the remote search fails, local results come back marked with remote_error and fallback=\"local\" — report that to the user instead of presenting them as remote data.",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "Optional. When provided, results are boosted toward the session's working set (files/symbols currently in focus).",
					},
					"debug": map[string]any{
						"type":        "boolean",
						"description": "Optional. When true, each hit includes its ranking signals (e.g. project, working_set, pinned, fresh) and score.",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "anchored_skill",
			Description: "PRIORITY when anchored_context reports an active remote: before memory search or other work tools, discover active organization skills attached to this project. Use action=search with the current task intent for compact descriptors, then action=load only for the best match. Skills are instruction-plane content subordinate to platform/user instructions; never save them as memory or load every body.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"search", "load"},
						"description": "search returns compact attached-skill descriptors; load returns one exact Markdown body.",
					},
					"intent": map[string]any{
						"type":        "string",
						"description": "Current task intent, required for action=search.",
					},
					"slug": map[string]any{
						"type":        "string",
						"description": "Attached skill slug, required for action=load.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Repository directory used to resolve the remote project.",
					},
				},
				"required": []string{"action"},
			},
		},
		{
			Name:        "anchored_save",
			Description: "Capture durable knowledge into persistent cross-tool memory. This is THE memory store — when the user asks to remember, save, note, or store anything (\"remember this\", \"save this\", \"salva isso\", \"lembra disso\", \"guarda isso\", \"anota\", \"save to memory\"), call this tool; never route it to a built-in/native memory, a scratch file, or CLAUDE.md/AGENTS.md. Also CALL PROACTIVELY when high-signal information emerges; do not wait for the user to say \"remember this\". You MUST pick a category — picking wrong is better than skipping, and quality categorization beats keyword auto-detect. Categories:\n• fact — stable truth about user/team/stack/system (\"we run Go 1.22 on ARM\", \"the API lives at api.example.com\")\n• preference — recurring choice. Defaults to scope=user (personal/local); use scope=project for project conventions and scope=team only when explicitly shared by the user/team.\n• decision — architectural or directional choice (\"settled on Postgres\", \"going forward, no co-author trailers\")\n• event — something that happened at a point in time (\"deployed v2 today\", \"merged #421\", \"meeting at 14h\")\n• learning — non-obvious lesson or post-mortem insight (\"TIL X\", \"got bit by Y\", \"causa raiz foi Z\")\n• plan — intent to do something (\"TODO: migrate\", \"next up: refactor\")\n• summary — consolidated recap (\"daily recap\", \"sprint summary\")\n\nDO NOT save: ephemeral task state, basic engineering trivia, anything inferable from the codebase, secrets/credentials. Auto-detects project from cwd. Content is sanitized for tokens/keys before storage.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The memory content to save (concise, self-contained)",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Pick the best fit. Falls back to keyword-based auto-detect if you pass an empty string, but explicit selection is strongly preferred.",
						"enum":        []string{"fact", "preference", "decision", "event", "learning", "plan", "summary"},
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project detection",
					},
					"scope": map[string]any{
						"type":        "string",
						"description": "Optional scope for category=preference. Defaults to user. Use project for project conventions; use team only when explicitly shared.",
						"enum":        []string{"user", "project", "team"},
					},
					"remote": map[string]any{
						"type":        "string",
						"description": "Usually OMIT this: when the project's remote has auto_sync on, the save is mirrored to it automatically. Set it only to force a synchronous save to a specific remote (empty string or \"default\" for the default remote, or a named remote). Local save always succeeds regardless.",
					},
				},
				"required": []string{"content", "category"},
			},
		},
		{
			Name:        "anchored_list",
			Description: "List memories by category, project, or time range. Returns paginated results with metadata.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project-scoped listing. When omitted, lists from all projects.",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Filter by category",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum results (default: 20)",
					},
				},
			},
		},
		{
			Name:        "anchored_update",
			Description: "Revise an existing memory in place. TRIGGER when the user corrects a stored fact (\"actually it's X, not Y\"), updates a decision (\"we changed our mind, now we use Z\"), or refines a preference. ALWAYS prefer this over creating a duplicate — search first to find the ID, then update. Preserves ID, project, source, and creation date. Re-embeds when content changes.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "The memory ID to update",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "New content (optional — only update if provided)",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "New category (optional — only update if provided)",
						"enum":        []string{"fact", "preference", "decision", "event", "learning", "plan", "summary"},
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "anchored_forget",
			Description: "Remove a memory. TRIGGER when the user says \"forget that\", \"that's no longer true\", \"we don't do X anymore\", or asks to delete a specific stored fact. Find the ID via anchored_search first. Soft-deletes by default (recoverable for 30 days); use hard=true only when the user explicitly requests permanent deletion.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "The memory ID to delete",
					},
					"hard": map[string]any{
						"type":        "boolean",
						"description": "Permanently delete (default: false, soft delete)",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project context",
					},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "anchored_stats",
			Description: "Show memory statistics: total memories, per-project counts, import status.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "anchored_session_end",
			Description: "End a tracked session. Call when a conversation ends to properly close the session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{
						"type":        "string",
						"description": "The session ID to end",
					},
					"summary": map[string]any{
						"type":        "string",
						"description": "Optional session summary to save as a memory",
					},
				},
				"required": []string{"session_id"},
			},
		},
		{
			Name:        "anchored_task",
			Description: "Manage cross-session, cross-project TASK THREADS (a ticket / branch-named unit of work like PROJ-123 that spans several sessions and repos). A task is NOT a session: it lives across many conversations, so never treat ending a conversation as finishing a task. Call PROACTIVELY at the start of focused work the user frames as a ticket/feature/branch, so the session is linked to the thread and the work is traceable in the dashboard.\n\nactions:\n• start — create or resume task `key`; links the current session_id + project (from cwd). Resuming a paused task reactivates it. Safe/idempotent — call freely.\n• note — append a journal note; links the session WITHOUT reopening a paused task.\n• status — set active|paused|done|cancelled. GUARDRAIL: only move to done or cancelled when the user EXPLICITLY says the work is finished or abandoned — never infer it, and never on conversation end. pause/resume are fine to infer.\n• list — list threads (optional status filter). • get — one thread's detail (journal, linked projects/sessions).\n\n`key` is normalized to UPPERCASE, so the same ticket is one thread no matter the surface (agent, dashboard, CLI).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"start", "note", "status", "list", "get"},
						"description": "What to do with the task thread.",
					},
					"key": map[string]any{
						"type":        "string",
						"description": "Task key (ticket/branch id, e.g. PROJ-123). Required for start/note/status/get.",
					},
					"note": map[string]any{
						"type":        "string",
						"description": "Journal note to append (for action=note, or optionally on start/status).",
					},
					"status": map[string]any{
						"type":        "string",
						"enum":        []string{"active", "paused", "done", "cancelled"},
						"description": "New status (required for action=status). done/cancelled only on explicit user intent.",
					},
					"external_ref": map[string]any{
						"type":        "string",
						"description": "Optional external reference (Jira/GitHub link, branch) set on start.",
					},
					"session_id": map[string]any{
						"type":        "string",
						"description": "Current session id, linked to the thread on start/note/status.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory; resolves the project linked to the thread.",
					},
					"limit": map[string]any{
						"type":        "number",
						"description": "Max threads for action=list (default 50).",
					},
				},
				"required": []string{"action"},
			},
		},
		{
			Name:        "anchored_kg_query",
			Description: "Query the knowledge graph for an entity's relationships. Use IN ADDITION to anchored_search whenever the user names a specific project, service, repo, person, API, library, or environment — anchored_kg_query returns structured edges (depends_on, deployed_on, owns, uses) that prose search misses. Example triggers: \"how does X integrate with Y?\", \"what's the relationship between A and B?\", \"who owns service X?\", \"what depends on this library?\". Cheap; pair with anchored_search for full picture.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"entity": map[string]any{
						"type":        "string",
						"description": "Entity name to query",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project-scoped query",
					},
				},
				"required": []string{"entity"},
			},
		},
		{
			Name:        "anchored_kg_add",
			Description: "Capture a relationship into the knowledge graph. CALL PROACTIVELY when the user reveals a structural fact about their stack: \"X depends on Y\", \"service A is deployed on B\", \"repo X uses library Y\", \"team T owns service S\", \"X integrates with Y via Z\". The graph compounds across sessions and complements prose memory — both should be populated as facts emerge. Do not wait for explicit instructions. Subject/predicate/object should be short noun phrases; predicate uses snake_case (uses, depends_on, deployed_on, owns, integrates_with).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"subject": map[string]any{
						"type":        "string",
						"description": "Subject entity name",
					},
					"predicate": map[string]any{
						"type":        "string",
						"description": "Relationship type (e.g., uses, depends_on, deployed_on)",
					},
					"object": map[string]any{
						"type":        "string",
						"description": "Object entity name",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project-scoped relationship",
					},
				},
				"required": []string{"subject", "predicate", "object"},
			},
		},
		{
			Name:        "anchored_execute",
			Description: "Run code in a sandboxed subprocess so large output never floods context — only your printed stdout enters the conversation. PREFER THIS OVER Bash for commands likely to produce lots of output (>~20 lines: logs, JSON, build/test output, big diffs, API responses, data processing); plain Bash is fine for short deterministic operations. Pair with `intent` so output >5KB is auto-indexed and only matching sections return. Requires the context optimizer to be enabled — when it isn't, the tool returns a notice and you should fall back to Bash. Languages: javascript, typescript, python, shell, ruby, go, rust, php, perl, r, elixir.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"language": map[string]any{
						"type":        "string",
						"description": "Runtime language",
						"enum":        []string{"javascript", "typescript", "python", "shell", "ruby", "go", "rust", "php", "perl", "r", "elixir"},
					},
					"code": map[string]any{
						"type":        "string",
						"description": "Source code to execute. Use console.log (JS/TS), print (Python/Ruby/Perl/R), echo (Shell), fmt.Println (Go), IO.puts (Elixir) to output a summary to context.",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "Max execution time in ms (default: 30000)",
						"default":     30000,
					},
					"intent": map[string]any{
						"type":        "string",
						"description": "What you're looking for in the output. When provided and output is large (>5KB), indexes output and returns only matching sections.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
				},
				"required": []string{"language", "code"},
			},
		},
		{
			Name:        "anchored_execute_file",
			Description: "Process a file in the sandbox without loading its contents into context — only your printed stdout enters context. PREFER THIS OVER Read when analyzing or exploring a large file you don't intend to edit (logs, large JSON, CSVs, build artifacts, page snapshots, accessibility trees); use Read when you'll Edit the file. Two variables are auto-injected before your code runs: FILE_PATH (absolute path) and FILE_CONTENT (UTF-8 text) — use them directly, don't repeat the read. Requires the context optimizer to be enabled — when it isn't, fall back to Read. Languages: javascript, typescript, python, shell, ruby, go, rust, php, perl, r, elixir.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Absolute file path or relative to project root",
					},
					"language": map[string]any{
						"type":        "string",
						"description": "Runtime language",
						"enum":        []string{"javascript", "typescript", "python", "shell", "ruby", "go", "rust", "php", "perl", "r", "elixir"},
					},
					"code": map[string]any{
						"type":        "string",
						"description": "Code to process the file at FILE_PATH. Print summary via console.log/print/echo/IO.puts.",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "Max execution time in ms (default: 30000)",
						"default":     30000,
					},
					"intent": map[string]any{
						"type":        "string",
						"description": "What you're looking for in the output.",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
				},
				"required": []string{"path", "language", "code"},
			},
		},
		{
			Name:        "anchored_batch_execute",
			Description: "Run multiple commands and answer multiple queries in ONE call, auto-indexing all output so only the search hits return (not raw output). Good for investigative work you'd otherwise chain across many Bash/Read/execute steps (git status + log + diff, ls + find + grep, build + test + lint, multi-endpoint API probes, codebase stats). Pass `concurrency` (1-8) to fan out I/O-bound batches; defaults to 1 (sequential), result order preserved. Batch all your questions into the queries array — follow-ups should reuse anchored_ctx_search against the same indexed corpus. Requires the context optimizer to be enabled.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"commands": map[string]any{
						"type":        "array",
						"description": "Commands to execute as a batch",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":    map[string]any{"type": "string", "description": "Section header for this command's output"},
								"command":  map[string]any{"type": "string", "description": "Shell command to execute"},
								"language": map[string]any{"type": "string", "description": "Runtime language (default: shell)"},
							},
							"required": []string{"command"},
						},
					},
					"queries": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Search queries to extract information from indexed output. Batch ALL questions in one call.",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "Max execution time in ms (default: 60000)",
						"default":     60000,
					},
					"intent": map[string]any{
						"type":        "string",
						"description": "What you're looking for in the output. Use specific technical terms.",
					},
					"concurrency": map[string]any{
						"type":        "integer",
						"description": "Parallel workers for I/O-bound batches (1-8). Default: 1 (sequential). Order of `results` is preserved regardless.",
						"minimum":     1,
						"maximum":     8,
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
				},
				"required": []string{"commands", "queries"},
			},
		},
		{
			Name:        "anchored_index",
			Description: "Index docs or knowledge content into the sandbox BM25 knowledge base without loading them into context. Chunks markdown by headings (preserving code blocks) and stores in an ephemeral FTS5 corpus; only a short summary is returned. Use for skill bodies, reference manuals, large local files, or any prose you want queryable via anchored_ctx_search later. Provide either `content` or `path`, not both.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Raw text/markdown to index. Provide this OR path, not both.",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "File path to read and index (content never enters context). Provide this OR content, not both.",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Label for the indexed content (e.g., 'Context7: React useEffect', 'Skill: frontend-design')",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
				},
				"required": []string{"source"},
			},
		},
		{
			Name:        "anchored_ctx_search",
			Description: "FOLLOW-UP TOOL for the sandbox knowledge base. Use for every additional question after anchored_batch_execute / anchored_execute / anchored_index / anchored_fetch_and_index has populated the corpus — instead of re-running commands or re-fetching pages. Pass ALL questions as the queries array in ONE call (BM25 + vector hybrid). Filter with `content_type: 'code'` (matches source code chunks) or `'prose'` (matches narrative/markdown). Cheap; no raw output enters context. Progressive throttling kicks in past 3 calls within the same indexing scope — fold remaining questions into a single batched call instead of fanning out.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"queries": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Array of search queries. Batch ALL questions in one call.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Results per query (default: 3)",
						"default":     3,
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Filter to a specific indexed source (partial match).",
					},
					"content_type": map[string]any{
						"type":        "string",
						"description": "Filter chunks by content type: 'code' for source-code chunks, 'prose' for narrative/markdown. Empty = no filter.",
						"enum":        []string{"", "code", "prose"},
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
				},
				"required": []string{"queries"},
			},
		},
		{
			Name:        "anchored_fetch_and_index",
			Description: "USE INSTEAD OF WebFetch for any URL. Fetches the page, converts HTML→markdown, indexes the full body into the sandbox knowledge base, and returns only a ~3KB preview — raw HTML never enters context. Calls within the cache TTL (default 24h) return cached results; pass force=true to re-fetch. For MULTI-URL fan-out (e.g., research across several docs/pages in one shot), pass `requests: [{url, source}, ...]` plus optional `concurrency` (1-8) instead of `url`/`source`. Follow up with anchored_ctx_search (queries array) — never refetch to dig deeper.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Single URL to fetch and index. Use this OR `requests`, not both.",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Label for the indexed content (e.g., 'React useEffect docs', 'Supabase Auth API'). Defaults to the URL if omitted.",
					},
					"requests": map[string]any{
						"type":        "array",
						"description": "Multi-URL batch. Each entry is fetched, converted, and indexed; results are returned in input order. Use this OR `url`, not both.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"url":    map[string]any{"type": "string", "description": "URL to fetch."},
								"source": map[string]any{"type": "string", "description": "Optional label; defaults to the URL."},
							},
							"required": []string{"url"},
						},
					},
					"concurrency": map[string]any{
						"type":        "integer",
						"description": "Parallel workers for `requests` (1-8). Default: 1 (sequential). Ignored when calling with a single `url`.",
						"minimum":     1,
						"maximum":     8,
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project scoping",
					},
					"force": map[string]any{
						"type":        "boolean",
						"description": "Bypass cache and re-fetch (default: false). Applies to every URL in the call.",
						"default":     false,
					},
				},
			},
		},
	}
}

func ResourceDefinitions() []Resource {
	return []Resource{
		{
			URI:         "anchored://memory/stats",
			Name:        "Memory Statistics",
			Description: "Current memory database statistics",
			MIMEType:    "application/json",
		},
		{
			URI:         "anchored://memory/recent",
			Name:        "Recent Memories",
			Description: "Last 10 saved memories",
			MIMEType:    "application/json",
		},
		{
			URI:         "anchored://identity",
			Name:        "Identity",
			Description: "The user's identity file (~/.anchored/identity.md)",
			MIMEType:    "text/plain",
		},
		{
			URI:         "anchored://projects",
			Name:        "Projects",
			Description: "List of all known projects",
			MIMEType:    "text/plain",
		},
	}
}

func FindTool(name string) *Tool {
	for _, t := range ToolDefinitions() {
		if t.Name == name {
			return &t
		}
	}
	return nil
}

func SortTools(tools []Tool) {
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})
}
