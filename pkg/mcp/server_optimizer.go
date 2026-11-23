package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// OptimizerFacade decouples pkg/mcp from pkg/context (which has a !windows build tag).
type OptimizerFacade interface {
	Execute(ctx context.Context, code string, language string, timeoutMs int, projectID string) (stdout string, stderr string, exitCode int, duration string, timedOut bool, truncated bool, err error)
	ExecuteFile(ctx context.Context, path string, language string, code string, timeoutMs int, projectID string) (stdout string, stderr string, exitCode int, duration string, timedOut bool, truncated bool, err error)
	IndexContent(ctx context.Context, content string, source string, label string, projectID string) (string, error)
	IndexRaw(ctx context.Context, content string, source string, label string, projectID string) (string, error)
	Search(ctx context.Context, query string, maxResults int, contentType string, source string, projectID string) ([]OptimizerSearchResult, error)
	FetchAndIndex(ctx context.Context, url string, source string, projectID string, force bool) (markdown string, fetchedAt string, fromCache bool, err error)
	FetchAndIndexBatch(ctx context.Context, requests []OptimizerFetchRequest, concurrency int, projectID string, force bool) ([]OptimizerFetchBatchEntry, error)
	ExecuteBatch(ctx context.Context, commands []OptimizerBatchCommand, queries []string, intent string, projectID string, concurrency int) (*OptimizerBatchResult, error)
	Close()
}

// OptimizerFetchRequest is a platform-independent fetch request for batch URL fetches.
type OptimizerFetchRequest struct {
	URL    string
	Source string
}

// OptimizerFetchBatchEntry is a platform-independent per-URL outcome.
type OptimizerFetchBatchEntry struct {
	URL       string
	Source    string
	Bytes     int
	FetchedAt string
	FromCache bool
	Error     string
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

func (s *Server) toolCtxExecute(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Language string `json:"language"`
		Code     string `json:"code"`
		Timeout  int    `json:"timeout"`
		Intent   string `json:"intent"`
		CWD      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 30000
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	stdout, stderr, exitCode, dur, timedOut, truncated, err := s.optimizer.Execute(ctx, p.Code, p.Language, p.Timeout, projectID)
	if err != nil {
		return "", err
	}
	if timedOut {
		return fmt.Sprintf("TIMEOUT after %s", dur), nil
	}
	if exitCode != 0 {
		return fmt.Sprintf("ERROR (exit %d): %s", exitCode, headTail(stderr, execErrHead, execErrTail)), nil
	}
	output := stdout
	if truncated {
		output += "\n[output truncated by the sandbox cap]"
	}
	return s.renderExecOutput(ctx, output, stdout, p.Intent, projectID, dur), nil
}

// renderExecOutput bounds a successful execution's reply. Small outputs go
// back verbatim; anything over execInlineBytes is indexed (so it stays
// queryable) and answered with intent-matched sections or a head/tail
// preview — never the full dump, which the harness would persist to a file
// the agent then re-reads in slices or gives up on.
func (s *Server) renderExecOutput(ctx context.Context, output, raw, intent, projectID, dur string) string {
	if len(output) <= execInlineBytes {
		return fmt.Sprintf("```\n%s\n```\nExit: 0 (%s)", output, dur)
	}
	if _, idxErr := s.optimizer.IndexRaw(ctx, raw, "execute", "auto-indexed", projectID); idxErr != nil {
		s.logger.Warn("ctx_execute: failed to index output", "error", idxErr)
	}
	if intent != "" {
		hits, sErr := s.optimizer.Search(ctx, intent, 5, "", "", projectID)
		if sErr == nil && len(hits) > 0 {
			var lines []string
			for i, r := range hits {
				lines = append(lines, fmt.Sprintf("%d. [%s] %s", i+1, r.Label, r.Snippet))
			}
			return fmt.Sprintf("Large output indexed (%d bytes). Matching sections:\n\n%s", len(raw), joinLines(lines))
		}
		return fmt.Sprintf("Large output indexed (%d bytes). No sections matched intent — query specifics with anchored_ctx_search(queries=[...]).", len(raw))
	}
	return fmt.Sprintf("Output indexed (%d bytes) — query specifics with anchored_ctx_search(queries=[...]). Preview:\n```\n%s\n```\nExit: 0 (%s)",
		len(raw), headTail(output, execPreviewHead, execPreviewTail), dur)
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
		CWD      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 30000
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	// Optimizer.ExecuteFile injects FILE_PATH and FILE_CONTENT preludes per
	// language; we just forward the user's path and code as-is.
	stdout, stderr, exitCode, dur, timedOut, truncated, err := s.optimizer.ExecuteFile(ctx, s.rootedFile(ctx, p.CWD, p.Path), p.Language, p.Code, p.Timeout, projectID)
	if err != nil {
		return "", err
	}
	if timedOut {
		return fmt.Sprintf("TIMEOUT after %s", dur), nil
	}
	if exitCode != 0 {
		return fmt.Sprintf("ERROR (exit %d): %s", exitCode, headTail(stderr, execErrHead, execErrTail)), nil
	}
	output := stdout
	if truncated {
		output += "\n[output truncated by the sandbox cap]"
	}
	return s.renderExecOutput(ctx, output, stdout, p.Intent, projectID, dur), nil
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
		Queries     []string `json:"queries"`
		Timeout     int      `json:"timeout"`
		Intent      string   `json:"intent"`
		Concurrency int      `json:"concurrency"`
		CWD         string   `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Timeout == 0 {
		p.Timeout = 60000
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	cmds := make([]OptimizerBatchCommand, len(p.Commands))
	for i, c := range p.Commands {
		cmds[i] = OptimizerBatchCommand{
			Label:    c.Label,
			Command:  c.Command,
			Language: batchLanguage(c.Language),
		}
	}
	s.resetSearchThrottle(ctx)
	// The batch holds one execution slot per command it runs at once; it
	// cannot run more than the session's limit (see executionWeight).
	result, err := s.optimizer.ExecuteBatch(ctx, cmds, p.Queries, p.Intent, projectID, min(p.Concurrency, executionLimit()))
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
			lines = append(lines, fmt.Sprintf("\nCommand %d failed (exit %d): %s",
				i+1, r.ExitCode, headTail(r.Stderr, execErrHead, execErrTail)))
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
		CWD     string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	if p.Content != "" {
		id, err := s.optimizer.IndexContent(ctx, p.Content, p.Source, "manual", projectID)
		if err != nil {
			return "", err
		}
		s.resetSearchThrottle(ctx)
		return fmt.Sprintf("Indexed content from '%s' (id: %s)", p.Source, id), nil
	}
	if p.Path != "" {
		p.Path = s.rootedFile(ctx, p.CWD, p.Path)
		data, err := os.ReadFile(p.Path)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		id, err := s.optimizer.IndexContent(ctx, string(data), p.Source, p.Path, projectID)
		if err != nil {
			return "", err
		}
		s.resetSearchThrottle(ctx)
		return fmt.Sprintf("Indexed file '%s' as '%s' (id: %s)", p.Path, p.Source, id), nil
	}
	return "", fmt.Errorf("provide either 'content' or 'path'")
}

func (s *Server) toolCtxSearch(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		Queries     []string `json:"queries"`
		Limit       int      `json:"limit"`
		Source      string   `json:"source"`
		ContentType string   `json:"content_type"`
		CWD         string   `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.Limit == 0 {
		p.Limit = 3
	}
	if p.ContentType != "" && p.ContentType != "code" && p.ContentType != "prose" {
		return "", fmt.Errorf("content_type must be 'code', 'prose', or empty (got %q)", p.ContentType)
	}

	// Progressive throttling — encourages folding follow-ups into the next
	// anchored_batch_execute / anchored_fetch_and_index call instead of fanning
	// out one query per round-trip.
	call := s.nextSearchCall(ctx)
	if call >= 9 {
		return "anchored_ctx_search throttled: 9+ consecutive calls without re-indexing. Fold remaining questions into the queries array of anchored_batch_execute (or anchored_fetch_and_index) so output is captured and searched in one round-trip.", nil
	}
	limit := p.Limit
	throttleNote := ""
	if call >= 4 {
		limit = 1
		throttleNote = fmt.Sprintf("\n\n[throttle] call #%d — results reduced to 1/query. Batch follow-ups via anchored_batch_execute(queries=[...]).", call)
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	seen := make(map[string]bool)
	var lines []string
	for _, q := range p.Queries {
		hits, err := s.optimizer.Search(ctx, q, limit, p.ContentType, p.Source, projectID)
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
		return "No results found for any query." + throttleNote, nil
	}
	return joinLines(lines) + throttleNote, nil
}

func (s *Server) toolCtxFetchAndIndex(ctx context.Context, args json.RawMessage) (string, error) {
	if s.optimizer == nil {
		return "Context optimizer not enabled. Set context_optimizer.enabled: true in config.", nil
	}
	var p struct {
		URL      string `json:"url"`
		Source   string `json:"source"`
		Requests []struct {
			URL    string `json:"url"`
			Source string `json:"source"`
		} `json:"requests"`
		Concurrency int    `json:"concurrency"`
		CWD         string `json:"cwd"`
		Force       bool   `json:"force"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if p.URL == "" && len(p.Requests) == 0 {
		return "", fmt.Errorf("provide either 'url' or 'requests'")
	}
	if p.URL != "" && len(p.Requests) > 0 {
		return "", fmt.Errorf("provide either 'url' or 'requests', not both")
	}

	projectID := s.optionalProjectID(ctx, p.CWD)

	s.resetSearchThrottle(ctx)

	if len(p.Requests) > 0 {
		reqs := make([]OptimizerFetchRequest, len(p.Requests))
		for i, r := range p.Requests {
			reqs[i] = OptimizerFetchRequest{URL: r.URL, Source: r.Source}
		}
		entries, err := s.optimizer.FetchAndIndexBatch(ctx, reqs, min(p.Concurrency, executionLimit()), projectID, p.Force)
		if err != nil {
			return "", err
		}
		var ok, failed, cached, totalBytes int
		var lines []string
		for i, e := range entries {
			if e.Error != "" {
				failed++
				lines = append(lines, fmt.Sprintf("%d. [%s] FAILED — %s", i+1, e.URL, e.Error))
				continue
			}
			ok++
			totalBytes += e.Bytes
			cacheStatus := ""
			if e.FromCache {
				cached++
				cacheStatus = " (from cache)"
			}
			lines = append(lines, fmt.Sprintf("%d. [%s] %s%s — %d bytes at %s", i+1, e.Source, e.URL, cacheStatus, e.Bytes, e.FetchedAt))
		}
		header := fmt.Sprintf("Fetched %d URL(s): %d ok (%d cached), %d failed. Indexed %d bytes total.\nUse anchored_ctx_search to query the corpus.", len(entries), ok, cached, failed, totalBytes)
		return header + "\n\n" + joinLines(lines), nil
	}

	source := p.Source
	if source == "" {
		source = p.URL
	}
	markdown, fetchedAt, fromCache, err := s.optimizer.FetchAndIndex(ctx, p.URL, source, projectID, p.Force)
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
	return fmt.Sprintf("Fetched and indexed '%s'%s at %s (%d bytes).\n\n%s\n\nUse anchored_ctx_search to find specific sections.", source, cacheStatus, fetchedAt, len(markdown), preview), nil
}

// batchLanguage is a batch command's language: shell when it names none, as
// the schema says.
func batchLanguage(language string) string {
	if language == "" {
		return "shell"
	}
	return language
}
