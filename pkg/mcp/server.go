package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
)

type Server struct {
	mem      *memory.Service
	kg       *kg.KG
	sessions *session.Manager
	logger   *slog.Logger
	version  string
}

func NewServer(mem *memory.Service, kg *kg.KG, sessions *session.Manager, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{mem: mem, kg: kg, sessions: sessions, logger: logger, version: version}
}

func (s *Server) HandleMessage(ctx context.Context, data []byte) []byte {
	req, err := ParseRequest(data)
	if err != nil {
		return MarshalResponse(NewErrorResponse(nil, NewError(-32700, err.Error())))
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID, req.Params)
	case "notifications/initialized":
		return nil
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(ctx, req.ID, req.Params)
	case "resources/list":
		return s.handleResourcesList(req.ID)
	case "resources/read":
		return s.handleResourcesRead(ctx, req.ID, req.Params)
	case "ping":
		return MarshalResponse(NewResponse(req.ID, map[string]string{}))
	default:
		return MarshalResponse(NewErrorResponse(req.ID, NewError(-32601, fmt.Sprintf("unknown method: %s", req.Method))))
	}
}

func (s *Server) handleInitialize(id json.RawMessage, params json.RawMessage) []byte {
	result := InitializeResult{
		ProtocolVersion: MCPVersion,
		ServerInfo: ServerInfo{
			Name:    "anchored",
			Version: s.version,
		},
		Instructions: "Anchored provides persistent cross-tool memory. Call anchored_context at conversation start. Use anchored_save to store facts and anchored_search to retrieve them. Never save secrets.",
	}
	result.Capabilities.Tools.ListChanged = false
	result.Capabilities.Resources.Subscribe = false
	result.Capabilities.Resources.ListChanged = false

	return MarshalResponse(NewResponse(id, result))
}

func (s *Server) handleToolsList(id json.RawMessage) []byte {
	tools := ToolDefinitions()
	SortTools(tools)
	return MarshalResponse(NewResponse(id, map[string]any{"tools": tools}))
}

func (s *Server) handleToolsCall(ctx context.Context, id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return MarshalResponse(NewErrorResponse(id, InvalidParams("invalid params")))
	}

	result, err := s.callTool(ctx, p.Name, p.Arguments)
	if err != nil {
		return MarshalResponse(NewErrorResponse(id, InternalError(err)))
	}

	return MarshalResponse(NewResponse(id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": result},
		},
	}))
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "anchored_context":
		return s.toolContext(ctx, args)
	case "anchored_search":
		return s.toolSearch(ctx, args)
	case "anchored_save":
		return s.toolSave(ctx, args)
	case "anchored_list":
		return s.toolList(ctx, args)
	case "anchored_forget":
		return s.toolForget(ctx, args)
	case "anchored_update":
		return s.toolUpdate(ctx, args)
	case "anchored_stats":
		return s.toolStats(ctx)
	case "kg_query":
		return s.toolKGQuery(ctx, args)
	case "kg_add":
		return s.toolKGAdd(ctx, args)
	case "anchored_session_end":
		return s.toolSessionEnd(ctx, args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) toolContext(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		CWD       string `json:"cwd"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		p.CWD = "."
	}

	// Track session activity
	if s.sessions != nil && p.SessionID != "" {
		_ = s.sessions.RecordActivity(ctx, p.SessionID)
	}

	return "No memory context available yet. Save memories with anchored_save.", nil
}

func (s *Server) toolSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Query      string `json:"query"`
		CWD        string `json:"cwd"`
		Category   string `json:"category"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID, boostProjectID string
	if p.CWD != "" {
		projectID = s.mem.ResolveProject(p.CWD)
		boostProjectID = projectID
	}

	results, err := s.mem.Search(ctx, p.Query, memory.SearchOptions{
		MaxResults:    p.MaxResults,
		Category:      p.Category,
		ProjectID:     projectID,
		BoostProjectID: boostProjectID,
	})
	if err != nil {
		return "", err
	}

	if len(results) == 0 {
		return "No matching memories found.", nil
	}

	globalMode := p.CWD == ""
	var lines []string
	for i, r := range results {
		line := fmt.Sprintf("%d. [%s] %.3f — %s", i+1, r.Memory.Category, r.Score, r.Memory.Content)
		if globalMode && r.Memory.ProjectID != nil && *r.Memory.ProjectID != "" {
			line = fmt.Sprintf("%d. [project:%s] [%s] %.3f — %s", i+1, *r.Memory.ProjectID, r.Memory.Category, r.Score, r.Memory.Content)
		}
		lines = append(lines, line)
	}

	return fmt.Sprintf("Found %d memories:\n\n%s", len(results), joinLines(lines)), nil
}

func (s *Server) toolSave(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Content  string `json:"content"`
		Category string `json:"category"`
		CWD      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if p.CWD == "" {
		p.CWD = "."
	}

	m, err := s.mem.Save(ctx, p.Content, p.Category, "mcp", p.CWD)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Saved [%s] memory %s", m.Category, m.ID), nil
}

func (s *Server) toolList(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		CWD      string `json:"cwd"`
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID string
	if p.CWD != "" {
		projectID = s.mem.ResolveProject(p.CWD)
	}

	memories, err := s.mem.List(ctx, memory.ListOptions{
		Category:  p.Category,
		Limit:     p.Limit,
		ProjectID: projectID,
	})
	if err != nil {
		return "", err
	}

	if len(memories) == 0 {
		return "No memories found.", nil
	}

	var lines []string
	for i, m := range memories {
		lines = append(lines, fmt.Sprintf("%d. [%s] %s — %s", i+1, m.Category, m.CreatedAt.Format("2006-01-02 15:04"), m.Content))
	}

	return fmt.Sprintf("Showing %d memories:\n\n%s", len(memories), joinLines(lines)), nil
}

func (s *Server) toolForget(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID  string `json:"id"`
		Hard bool  `json:"hard"`
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	_ = p.CWD

	if p.Hard {
		if err := s.mem.Forget(ctx, p.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Permanently deleted memory %s", p.ID), nil
	}

	if err := s.mem.SoftForget(ctx, p.ID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Soft-deleted memory %s", p.ID), nil
}

func (s *Server) toolUpdate(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID       string `json:"id"`
		Content  string `json:"content"`
		Category string `json:"category"`
		CWD      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	_ = p.CWD

	m, err := s.mem.Update(ctx, p.ID, p.Content, p.Category)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Updated [%s] memory %s", m.Category, m.ID), nil
}

func (s *Server) toolStats(ctx context.Context) (string, error) {
	stats, err := s.mem.Stats(ctx)
	if err != nil {
		return "", err
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Total memories: %d", stats.TotalMemories))

	if len(stats.ByCategory) > 0 {
		lines = append(lines, "\nBy category:")
		for cat, count := range stats.ByCategory {
			lines = append(lines, fmt.Sprintf("  %s: %d", cat, count))
		}
	}

	if len(stats.ByProject) > 0 {
		lines = append(lines, "\nBy project:")
		for proj, count := range stats.ByProject {
			lines = append(lines, fmt.Sprintf("  %s: %d", proj, count))
		}
	}

	if s.sessions != nil {
		total, active, err := s.sessions.SessionStats(ctx)
		if err == nil {
			lines = append(lines, fmt.Sprintf("\nSessions: %d total, %d active", total, active))
		}
	}

	return joinLines(lines), nil
}

func (s *Server) toolSessionEnd(ctx context.Context, args json.RawMessage) (string, error) {
	if s.sessions == nil {
		return "Session tracking not available.", nil
	}

	var p struct {
		SessionID string `json:"session_id"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if p.SessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}

	if err := s.sessions.EndSession(ctx, p.SessionID); err != nil {
		return "", err
	}

	if p.Summary != "" {
		_, err := s.mem.Save(ctx, p.Summary, "summary", "session_end", ".")
		if err != nil {
			return fmt.Sprintf("Session %s ended (summary save failed: %v)", p.SessionID, err), nil
		}
		return fmt.Sprintf("Session %s ended with summary saved.", p.SessionID), nil
	}

	return fmt.Sprintf("Session %s ended.", p.SessionID), nil
}

func (s *Server) toolKGQuery(ctx context.Context, args json.RawMessage) (string, error) {
	if s.kg == nil {
		return "Knowledge graph not available.", nil
	}

	var p struct {
		Entity string `json:"entity"`
		CWD    string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID *string
	if pid := s.mem.ResolveProject(p.CWD); pid != "" {
		projectID = &pid
	}

	triples, err := s.kg.Query(ctx, p.Entity, projectID)
	if err != nil {
		return "", err
	}

	if len(triples) == 0 {
		return fmt.Sprintf("No relationships found for \"%s\".", p.Entity), nil
	}

	var lines []string
	for _, t := range triples {
		lines = append(lines, fmt.Sprintf("• %s — %s → %s", t.Subject, t.Predicate, t.Object))
	}

	return joinLines(lines), nil
}

func (s *Server) toolKGAdd(ctx context.Context, args json.RawMessage) (string, error) {
	if s.kg == nil {
		return "Knowledge graph not available.", nil
	}

	var p struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID *string
	if pid := s.mem.ResolveProject(p.CWD); pid != "" {
		projectID = &pid
	}

	triple, err := s.kg.AddTriple(ctx, p.Subject, p.Predicate, p.Object, projectID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Added relationship: %s — %s → %s (id: %s)", triple.Subject, triple.Predicate, triple.Object, triple.ID), nil
}

func (s *Server) handleResourcesList(id json.RawMessage) []byte {
	resources := ResourceDefinitions()
	return MarshalResponse(NewResponse(id, map[string]any{"resources": resources}))
}

func (s *Server) handleResourcesRead(ctx context.Context, id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return MarshalResponse(NewErrorResponse(id, InvalidParams("invalid params")))
	}

	var content string
	switch p.URI {
	case "anchored://memory/stats":
		stats, err := s.mem.Stats(ctx)
		if err != nil {
			return MarshalResponse(NewErrorResponse(id, InternalError(err)))
		}
		content = fmt.Sprintf("Total: %d\nCategories: %v\nProjects: %v",
			stats.TotalMemories, stats.ByCategory, stats.ByProject)
	case "anchored://memory/recent":
		memories, err := s.mem.List(ctx, memory.ListOptions{Limit: 10})
		if err != nil {
			return MarshalResponse(NewErrorResponse(id, InternalError(err)))
		}
		if len(memories) == 0 {
			content = "No memories yet."
		} else {
			var lines []string
			for _, m := range memories {
				lines = append(lines, fmt.Sprintf("[%s] %s", m.Category, m.Content))
			}
			content = joinLines(lines)
		}
	case "anchored://identity":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return MarshalResponse(NewErrorResponse(id, InternalError(err)))
		}
		identityPath := filepath.Join(homeDir, ".anchored", "identity.md")
		data, err := os.ReadFile(identityPath)
		if err != nil {
			content = "No identity file configured. Use 'anchored identity edit' to create one."
		} else {
			content = string(data)
		}
	case "anchored://projects":
		db := s.mem.StoreDB()
		rows, err := db.QueryContext(ctx, "SELECT id, name, path FROM projects ORDER BY name")
		if err != nil {
			return MarshalResponse(NewErrorResponse(id, InternalError(err)))
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var pid, name, ppath string
			if err := rows.Scan(&pid, &name, &ppath); err != nil {
				return MarshalResponse(NewErrorResponse(id, InternalError(err)))
			}
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s", pid, name, ppath))
		}
		if len(lines) == 0 {
			content = "No projects registered."
		} else {
			content = "ID\tName\tPath\n" + joinLines(lines)
		}
	default:
		return MarshalResponse(NewErrorResponse(id, NewError(-32601, fmt.Sprintf("unknown resource: %s", p.URI))))
	}

	return MarshalResponse(NewResponse(id, map[string]any{
		"contents": []map[string]any{
			{"uri": p.URI, "mimeType": "text/plain", "text": content},
		},
	}))
}

func joinLines(lines []string) string {
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}
