package mcp

import (
	"sort"
)

func ToolDefinitions() []Tool {
	return []Tool{
		{
			Name:        "anchored_context",
			Description: "ALWAYS call at conversation start. Returns your persistent identity + project context + relevant entity memories. This is your cross-tool memory — use it to load context before answering project questions.",
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
			Description: "Search your persistent cross-tool memory. Hybrid search (semantic vector + BM25 keyword). Use natural language queries. Returns ranked results with relevance scores.",
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
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "anchored_save",
			Description: "Save a memory permanently. Auto-detects current project. Content is sanitized (secrets removed automatically). NEVER save API keys, tokens, passwords, or credentials.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The memory content to save",
					},
					"category": map[string]any{
						"type":        "string",
						"description": "Category: fact, preference, decision, event, learning, plan (auto-detected if empty)",
						"enum":        []string{"fact", "preference", "decision", "event", "learning", "plan"},
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "Current working directory for project detection",
					},
				},
				"required": []string{"content"},
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
			Description: "Update an existing memory in-place. Preserves ID, project, and source. Re-embeds if content changed. Use to correct or evolve facts without losing context.",
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
						"enum":        []string{"fact", "preference", "decision", "event", "learning", "plan"},
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
			Description: "Remove a specific memory by ID. Soft-deletes by default (recoverable). Set hard=true for permanent deletion.",
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
			Name:        "kg_query",
			Description: "Query the knowledge graph. Find entities (projects, services, people, APIs) and their relationships.",
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
			Name:        "kg_add",
			Description: "Add a relationship to the knowledge graph. Use for structured knowledge like project dependencies, deployment targets, team members.",
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
			Description: "Execute code in a sandboxed subprocess. Only stdout enters context — raw data stays in the subprocess. Available: javascript, typescript, python, shell, ruby, go, rust, php, perl, r, elixir.",
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
				},
				"required": []string{"language", "code"},
			},
		},
		{
			Name:        "anchored_execute_file",
			Description: "Read a file and process it without loading contents into context. The file content is written to a temp file; FILE_PATH points to it. Only your printed summary enters context.",
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
				},
				"required": []string{"path", "language", "code"},
			},
		},
		{
			Name:        "anchored_batch_execute",
			Description: "Execute multiple commands in ONE call, auto-index all output, and search with multiple queries. Returns search results directly — no follow-up calls needed.",
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
				},
				"required": []string{"commands", "queries"},
			},
		},
		{
			Name:        "anchored_index",
			Description: "Index documentation or knowledge content into a searchable BM25 knowledge base. Chunks markdown by headings (keeping code blocks intact) and stores in ephemeral FTS5 database. The full content does NOT stay in context — only a brief summary is returned.",
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
				},
				"required": []string{"source"},
			},
		},
		{
			Name:        "anchored_ctx_search",
			Description: "Search indexed content. Requires prior indexing via anchored_index, anchored_execute, or anchored_batch_execute. Pass ALL search questions as queries array in ONE call.",
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
				},
				"required": []string{"queries"},
			},
		},
		{
			Name:        "anchored_fetch_and_index",
			Description: "Fetches URL content, converts HTML to markdown, indexes into searchable knowledge base, and returns a ~3KB preview. Full content stays in sandbox — use anchored_ctx_search for deeper lookups.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The URL to fetch and index",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Label for the indexed content (e.g., 'React useEffect docs', 'Supabase Auth API')",
					},
	
				},
				"required": []string{"url"},
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
