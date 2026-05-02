//go:build !windows

package mcp

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	ctxpkg "github.com/jholhewres/anchored/pkg/context"
	"github.com/jholhewres/anchored/pkg/config"
)

type ctxOptimizer struct {
	inner *ctxpkg.Optimizer
}

func NewCtxOptimizer(db *sql.DB, cfg config.ContextOptimizerConfig, logger *slog.Logger) (OptimizerFacade, error) {
	o, err := ctxpkg.NewOptimizer(db, cfg, logger)
	if err != nil {
		return nil, err
	}
	return &ctxOptimizer{inner: o}, nil
}

func (c *ctxOptimizer) Execute(ctx context.Context, code string, language string, timeoutMs int, projectID string) (string, string, int, string, bool, bool, error) {
	timeoutSec := timeoutMs / 1000
	r, err := c.inner.Execute(ctx, code, language, timeoutSec)
	if err != nil {
		return "", "", 0, "", false, false, err
	}
	return r.Stdout, r.Stderr, r.ExitCode, r.Duration.Round(time.Millisecond).String(), r.TimedOut, r.Truncated, nil
}

func (c *ctxOptimizer) ExecuteFile(ctx context.Context, path string, language string, code string, timeoutMs int, projectID string) (string, string, int, string, bool, bool, error) {
	timeoutSec := timeoutMs / 1000
	r, err := c.inner.ExecuteFile(ctx, path, language, code)
	if err != nil {
		return "", "", 0, "", false, false, err
	}
	_ = timeoutSec
	_ = projectID
	return r.Stdout, r.Stderr, r.ExitCode, r.Duration.Round(time.Millisecond).String(), r.TimedOut, r.Truncated, nil
}

func (c *ctxOptimizer) IndexContent(ctx context.Context, content string, source string, label string, projectID string) (string, error) {
	return c.inner.IndexContent(ctx, content, source, label, "prose", projectID)
}

func (c *ctxOptimizer) IndexRaw(ctx context.Context, content string, source string, label string, projectID string) (string, error) {
	return c.inner.IndexRaw(ctx, content, source, label, projectID)
}

func (c *ctxOptimizer) Search(ctx context.Context, query string, maxResults int, contentType string, source string, projectID string) ([]OptimizerSearchResult, error) {
	hits, err := c.inner.Search(ctx, query, maxResults, contentType, source, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]OptimizerSearchResult, len(hits))
	for i, h := range hits {
		out[i] = OptimizerSearchResult{
			ChunkID: h.ChunkID,
			Label:   h.Label,
			Source:  h.Source,
			Snippet: h.Snippet,
			Score:   h.Score,
		}
	}
	return out, nil
}

func (c *ctxOptimizer) FetchAndIndex(ctx context.Context, url string, source string, projectID string) (string, string, bool, error) {
	r, err := c.inner.FetchAndIndex(ctx, url, source, projectID)
	if err != nil {
		return "", "", false, err
	}
	return r.Markdown, r.FetchedAt.Format(time.RFC3339), r.FromCache, nil
}

func (c *ctxOptimizer) ExecuteBatch(ctx context.Context, commands []OptimizerBatchCommand, queries []string, intent string, projectID string) (*OptimizerBatchResult, error) {
	batchCmds := make([]ctxpkg.BatchCommand, len(commands))
	for i, c := range commands {
		batchCmds[i] = ctxpkg.BatchCommand{
			Label:    c.Label,
			Command:  c.Command,
			Language: c.Language,
		}
	}
	r, err := c.inner.ExecuteBatch(ctx, batchCmds, queries, intent, projectID)
	if err != nil {
		return nil, err
	}
	results := make([]OptimizerExecResult, len(r.Results))
	for i, er := range r.Results {
		results[i] = OptimizerExecResult{
			Stdout:    er.Stdout,
			Stderr:    er.Stderr,
			ExitCode:  er.ExitCode,
			Duration:  er.Duration.Round(time.Millisecond).String(),
			TimedOut:  er.TimedOut,
			Truncated: er.Truncated,
		}
	}
	searchResults := make([]OptimizerSearchResult, len(r.SearchResults))
	for i, sr := range r.SearchResults {
		searchResults[i] = OptimizerSearchResult{
			ChunkID: sr.ChunkID,
			Label:   sr.Label,
			Source:  sr.Source,
			Snippet: sr.Snippet,
			Score:   sr.Score,
		}
	}
	return &OptimizerBatchResult{
		Results:       results,
		SearchResults: searchResults,
		SourceID:      r.SourceID,
		TotalBytes:    r.TotalBytes,
	}, nil
}

func (c *ctxOptimizer) Close() {
	c.inner.Close()
}
