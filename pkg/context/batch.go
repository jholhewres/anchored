package ctx

import (
	"context"
	"log/slog"
	"strings"
)

const largeOutputThreshold = 5 * 1024 // 5 KB

// BatchExecutor runs multiple sandbox commands sequentially, indexes combined
// output, and optionally searches the indexed content.
type BatchExecutor struct {
	sandbox  *Sandbox
	indexer  *Indexer
	searcher *Searcher
	logger   *slog.Logger
}

// NewBatchExecutor creates a BatchExecutor. If logger is nil, slog.Default() is used.
func NewBatchExecutor(sandbox *Sandbox, indexer *Indexer, searcher *Searcher, logger *slog.Logger) *BatchExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	return &BatchExecutor{
		sandbox:  sandbox,
		indexer:  indexer,
		searcher: searcher,
		logger:   logger,
	}
}

// ExecuteBatch runs commands sequentially, indexes all combined output, and
// optionally searches the indexed content.
//
// Commands run one at a time. A failing command (non-zero exit, timeout) is
// recorded in results but does NOT abort the batch.
//
// If queries is non-empty, each query is run against the Searcher with
// MaxResults=5 and results are deduplicated by ChunkID.
//
// If the combined output exceeds 5 KB and intent is non-empty, all output is
// indexed but only search results matching intent terms are returned.
func (be *BatchExecutor) ExecuteBatch(ctx context.Context, commands []BatchCommand, queries []string, sessionID string, intent string, projectID string) (*BatchResult, error) {
	results := make([]ExecuteResult, 0, len(commands))
	var totalBytes int64
	var combined strings.Builder

	for _, cmd := range commands {
		res, err := be.sandbox.Execute(ctx, cmd.Language, cmd.Command)
		if err != nil {
			be.logger.Error("batch command infrastructure error",
				"label", cmd.Label,
				"error", err,
			)
			results = append(results, ExecuteResult{
				Stdout:   "",
				Stderr:   err.Error(),
				ExitCode: 1,
				TimedOut: false,
			})
			continue
		}

		results = append(results, *res)
		n := int64(len(res.Stdout))
		totalBytes += n

		if res.Stdout != "" {
			if combined.Len() > 0 {
				combined.WriteByte('\n')
			}
			combined.WriteString(res.Stdout)
		}

		if res.ExitCode != 0 || res.TimedOut {
			be.logger.Warn("batch command failed",
				"label", cmd.Label,
				"exit_code", res.ExitCode,
				"timed_out", res.TimedOut,
			)
		}
	}

	sourceID := ""
	if combined.Len() > 0 {
		id, err := be.indexer.IndexRaw(ctx, combined.String(), "batch", "batch-output", sessionID, projectID)
		if err != nil {
			be.logger.Error("batch index error", "error", err)
			return nil, err
		}
		sourceID = id
	}

	var searchResults []ContentSearchResult
	if len(queries) > 0 && be.searcher != nil {
		seen := make(map[string]bool)
		for _, q := range queries {
			if q == "" {
				continue
			}
			hits, err := be.searcher.Search(ctx, q, SearchOpts{MaxResults: 5, ProjectID: projectID})
			if err != nil {
				be.logger.Warn("batch search error", "query", q, "error", err)
				continue
			}
			for _, hit := range hits {
				if seen[hit.ChunkID] {
					continue
				}
				seen[hit.ChunkID] = true

				if combined.Len() > largeOutputThreshold && intent != "" {
					if !matchesIntent(hit.Snippet, intent) {
						continue
					}
				}

				searchResults = append(searchResults, hit)
			}
		}
	}

	return &BatchResult{
		Results:       results,
		SearchResults: searchResults,
		SourceID:      sourceID,
		TotalBytes:    totalBytes,
	}, nil
}

// matchesIntent returns true if the snippet contains at least one term from
// the intent string.
func matchesIntent(snippet string, intent string) bool {
	snippetLower := strings.ToLower(snippet)
	for _, term := range strings.Fields(strings.ToLower(intent)) {
		if strings.Contains(snippetLower, term) {
			return true
		}
	}
	return false
}
