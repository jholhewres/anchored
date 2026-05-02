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
