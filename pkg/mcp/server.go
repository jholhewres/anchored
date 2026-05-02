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

// OptimizerFacade decouples pkg/mcp from pkg/context (which has a !windows build tag).
type OptimizerFacade interface {
	Execute(ctx context.Context, code string, language string, timeoutMs int) (stdout string, stderr string, exitCode int, duration string, timedOut bool, truncated bool, err error)
	ExecuteFile(ctx context.Context, path string, language string, code string, timeoutMs int) (stdout string, stderr string, exitCode int, duration string, timedOut bool, truncated bool, err error)
	IndexContent(ctx context.Context, content string, source string, label string) (string, error)
	IndexRaw(ctx context.Context, content string, source string, label string) (string, error)
	Search(ctx context.Context, query string, maxResults int, contentType string, source string) ([]OptimizerSearchResult, error)
	FetchAndIndex(ctx context.Context, url string, source string) (markdown string, fetchedAt string, fromCache bool, err error)
	ExecuteBatch(ctx context.Context, commands []OptimizerBatchCommand, queries []string, intent string) (*OptimizerBatchResult, error)
	Close()
}

// OptimizerSearchResult is a platform-independent search result.
type OptimizerSearchResult struct {
	ChunkID string
	Label   string
	Source  string
	Snippet string
	Score   float64
}

// OptimizerBatchCommand is a platform-independent batch command.
type OptimizerBatchCommand struct {
	Label    string
	Command  string
	Language string
}

// OptimizerBatchResult is a platform-independent batch result.
type OptimizerBatchResult struct {
	Results       []OptimizerExecResult
	SearchResults []OptimizerSearchResult
	SourceID      string
	TotalBytes    int64
}

// OptimizerExecResult is a platform-independent exec result.
type OptimizerExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Duration  string
	TimedOut  bool
	Truncated bool
}

type Server struct {
	mem      *memory.Service
	kg       *kg.KG
	sessions *session.Manager
	optimizer OptimizerFacade
	logger   *slog.Logger
	version  string
}

func NewServer(mem *memory.Service, kg *kg.KG, sessions *session.Manager, optimizer OptimizerFacade, version string, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{mem: mem, kg: kg, sessions: sessions, optimizer: optimizer, logger: logger, version: version}
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
	case "anchored_execute":
		return s.toolCtxExecute(ctx, args)
	case "anchored_execute_file":
		return s.toolCtxExecuteFile(ctx, args)
	case "anchored_batch_execute":
		return s.toolCtxBatchExecute(ctx, args)
	case "anchored_index":
		return s.toolCtxIndex(ctx, args)
	case "anchored_ctx_search":
		return s.toolCtxSearch(ctx, args)
	case "anchored_fetch_and_index":
		return s.toolCtxFetchAndIndex(ctx, args)
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

func (s *Server) toolCtxExecute(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Language string `json:"language"`
		Code     string `json:"code"`
		Timeout  int    `json:"timeout"`
		Intent   string `json:"intent"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 30000
	}
	stdout, stderr, exitCode, dur, timedOut, truncated, err := s.optimizer.Execute(ctx, p.Code, p.Language, p.Timeout)
	if err != nil {
		return "", err
	}
	if timedOut {
		return fmt.Sprintf("TIMEOUT after %s", dur), nil
	}
	if exitCode != 0 {
		return fmt.Sprintf("ERROR (exit %d): %s", exitCode, stderr), nil
	}
	output := stdout
	if truncated {
		output += "\n[output truncated]"
	}
	if len(output) > 5*1024 && p.Intent != "" {
		_, _ = s.optimizer.IndexRaw(ctx, stdout, "execute", "auto-indexed")
		hits, sErr := s.optimizer.Search(ctx, p.Intent, 5, "", "")
		if sErr == nil && len(hits) > 0 {
			var lines []string
			for i, r := range hits {
				lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, r.Label, r.Snippet))
			}
			return fmt.Sprintf("Large output indexed (%d bytes). Matching sections:\n\n%s", len(stdout), joinLines(lines)), nil
		}
		return fmt.Sprintf("Large output indexed (%d bytes). No sections matched intent.", len(stdout)), nil
	}
	return fmt.Sprintf("```\n%s\n```\nExit: 0 (%s)", output, dur), nil
}

func (s *Server) toolCtxExecuteFile(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Path     string `json:"path"`
		Language string `json:"language"`
		Code     string `json:"code"`
		Timeout  int    `json:"timeout"`
		Intent   string `json:"intent"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 30000
	}
	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	// Write content to temp file to avoid cross-language string escaping issues.
	// FILE_PATH points to the temp file; the user's code reads from it.
	tmpFile, err := os.CreateTemp("", "anchored-fc-*.txt")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()
	wrapped := fmt.Sprintf("FILE_PATH=%q\n%s", tmpFile.Name(), p.Code)
	stdout, stderr, exitCode, dur, timedOut, truncated, err := s.optimizer.ExecuteFile(ctx, p.Path, p.Language, wrapped, p.Timeout)
	if err != nil {
		return "", err
	}
	if timedOut {
		return fmt.Sprintf("TIMEOUT after %s", dur), nil
	}
	if exitCode != 0 {
		return fmt.Sprintf("ERROR (exit %d): %s", exitCode, stderr), nil
	}
	output := stdout
	if truncated {
		output += "\n[output truncated]"
	}
	return fmt.Sprintf("```\n%s\n```\nExit: 0 (%s)", output, dur), nil
}

func (s *Server) toolCtxBatchExecute(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Commands []struct {
			Label    string `json:"label"`
			Command  string `json:"command"`
			Language string `json:"language"`
		} `json:"commands"`
		Queries []string `json:"queries"`
		Timeout int      `json:"timeout"`
		Intent  string   `json:"intent"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 60000
	}
	cmds := make([]OptimizerBatchCommand, len(p.Commands))
	for i, c := range p.Commands {
		cmds[i] = OptimizerBatchCommand{
			Label:    c.Label,
			Command:  c.Command,
			Language: c.Language,
		}
	}
	result, err := s.optimizer.ExecuteBatch(ctx, cmds, p.Queries, p.Intent)
	if err != nil {
		return "", err
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("Batch executed %d commands (%d bytes indexed).", len(result.Results), result.TotalBytes))
	if len(result.SearchResults) > 0 {
		lines = append(lines, "\nSearch results:")
		for i, r := range result.SearchResults {
			lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, r.Label, r.Snippet))
		}
	}
	for i, r := range result.Results {
		if r.ExitCode != 0 {
			lines = append(lines, fmt.Sprintf("\nCommand %d failed (exit %d): %s", i+1, r.ExitCode, r.Stderr))
		}
	}
	return joinLines(lines), nil
}

func (s *Server) toolCtxIndex(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Content string `json:"content"`
		Path    string `json:"path"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Content != "" {
		id, err := s.optimizer.IndexContent(ctx, p.Content, p.Source, "manual")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Indexed content from '%s' (id: %s)", p.Source, id), nil
	}
	if p.Path != "" {
		data, err := os.ReadFile(p.Path)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		id, err := s.optimizer.IndexContent(ctx, string(data), p.Source, p.Path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Indexed file '%s' as '%s' (id: %s)", p.Path, p.Source, id), nil
	}
	return "", fmt.Errorf("provide either 'content' or 'path'")
}

func (s *Server) toolCtxSearch(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Queries []string `json:"queries"`
		Limit   int      `json:"limit"`
		Source  string   `json:"source"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Limit == 0 {
		p.Limit = 3
	}
	seen := make(map[string]bool)
	var lines []string
	for _, q := range p.Queries {
		hits, err := s.optimizer.Search(ctx, q, p.Limit, "", p.Source)
		if err != nil {
			lines = append(lines, fmt.Sprintf("Query '%s': error — %v", q, err))
			continue
		}
		if len(hits) == 0 {
			lines = append(lines, fmt.Sprintf("Query '%s': no results.", q))
			continue
		}
		for _, h := range hits {
			if seen[h.ChunkID] {
				continue
			}
			seen[h.ChunkID] = true
			lines = append(lines, fmt.Sprintf("[%s] %.3f — %s", h.Source, h.Score, h.Snippet))
		}
	}
	if len(lines) == 0 {
		return "No results found for any query.", nil
	}
	return joinLines(lines), nil
}

func (s *Server) toolCtxFetchAndIndex(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		URL    string `json:"url"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Source == "" {
		p.Source = p.URL
	}
	markdown, fetchedAt, fromCache, err := s.optimizer.FetchAndIndex(ctx, p.URL, p.Source)
	if err != nil {
		return "", err
	}
	preview := markdown
	if len(preview) > 3*1024 {
		preview = preview[:3*1024] + "\n[...truncated preview...]"
	}
	cacheStatus := ""
	if fromCache {
		cacheStatus = " (from cache)"
	}
	return fmt.Sprintf("Fetched and indexed '%s'%s at %s (%d bytes).\n\n%s\n\nUse anchored_ctx_search to find specific sections.", p.Source, cacheStatus, fetchedAt, len(markdown), preview), nil
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
		if err := rows.Err(); err != nil {
			return MarshalResponse(NewErrorResponse(id, InternalError(err)))
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
