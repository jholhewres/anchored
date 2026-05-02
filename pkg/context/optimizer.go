//go:build !windows

package ctx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// Optimizer is the facade that MCP tools call. It composes all internal
// components (Store, Sandbox, Chunker, Searcher, Indexer, Fetcher,
// BatchExecutor, Evictor) and exposes a clean public API.
type Optimizer struct {
	store    *Store
	sandbox  *Sandbox
	chunker  *Chunker
	searcher *Searcher
	indexer  *Indexer
	fetcher  *Fetcher
	batch    *BatchExecutor
	evictor  *Evictor
	logger   *slog.Logger
	cfg      config.ContextOptimizerConfig
}

// NewOptimizer creates all internal components, wires them together, and
// starts the background evictor goroutine.
func NewOptimizer(db *sql.DB, cfg config.ContextOptimizerConfig, logger *slog.Logger) (*Optimizer, error) {
	if logger == nil {
		logger = slog.Default()
	}

	store := NewStore(db, logger)
	if err := store.PrepareStatements(); err != nil {
		return nil, fmt.Errorf("prepare statements: %w", err)
	}

	chunker := NewChunker(4096)

	sandboxTimeout := time.Duration(cfg.SandboxTimeout) * time.Second
	if sandboxTimeout <= 0 {
		sandboxTimeout = 30 * time.Second
	}
	maxOutputBytes := int64(cfg.MaxOutputKB) * 1024
	if maxOutputBytes <= 0 {
		maxOutputBytes = 1 << 20
	}
	sandbox := NewSandbox(sandboxTimeout, maxOutputBytes, "")

	searcher := NewSearcher(store, logger)

	indexer := NewIndexer(store, chunker, db, cfg.DefaultTTL, logger)

	fetchCacheTTL := time.Duration(cfg.FetchCacheTTL) * time.Hour
	if fetchCacheTTL <= 0 {
		fetchCacheTTL = 24 * time.Hour
	}
	fetcher := NewFetcher(30*time.Second, fetchCacheTTL, logger)

	batch := NewBatchExecutor(sandbox, indexer, searcher, logger)

	lruCapBytes := int64(cfg.LRUCapMB) * 1024 * 1024
	evictor := NewEvictor(store, db, EvictorConfig{
		TTLDefaultHours:  cfg.DefaultTTL,
		LRUCapBytes:      lruCapBytes,
		EvictionInterval: 10 * time.Minute,
	}, logger)

	o := &Optimizer{
		store:    store,
		sandbox:  sandbox,
		chunker:  chunker,
		searcher: searcher,
		indexer:  indexer,
		fetcher:  fetcher,
		batch:    batch,
		evictor:  evictor,
		logger:   logger,
		cfg:      cfg,
	}

	ctx := context.Background()
	o.evictor.Start(ctx)

	return o, nil
}

// Execute runs code in the sandbox. If timeoutSec > 0, a child context with
// that timeout is created; otherwise the config default is used (already baked
// into the Sandbox).
func (o *Optimizer) Execute(ctx context.Context, code string, language string, timeoutSec int) (*ExecuteResult, error) {
	if timeoutSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
		defer cancel()
	}
	return o.sandbox.Execute(ctx, language, code)
}

// ExecuteFile runs code in the sandbox. The path parameter is for
// metadata/logging only — the sandbox writes to temp files internally.
func (o *Optimizer) ExecuteFile(ctx context.Context, path string, language string, code string) (*ExecuteResult, error) {
	o.logger.Debug("execute file", "path", path, "language", language)
	return o.sandbox.Execute(ctx, language, code)
}

// IndexContent chunks markdown content and indexes it. Returns a sourceGroupID.
func (o *Optimizer) IndexContent(ctx context.Context, content string, source string, label string, contentType string) (string, error) {
	return o.indexer.IndexContent(ctx, content, source, label, "", contentType)
}

// IndexRaw indexes non-markdown content. Returns a sourceGroupID.
func (o *Optimizer) IndexRaw(ctx context.Context, content string, source string, label string) (string, error) {
	return o.indexer.IndexRaw(ctx, content, source, label, "")
}

// Search searches indexed content.
func (o *Optimizer) Search(ctx context.Context, query string, maxResults int, contentType string, source string) ([]ContentSearchResult, error) {
	return o.searcher.Search(ctx, query, SearchOpts{
		MaxResults:  maxResults,
		ContentType: contentType,
		Source:      source,
	})
}

// FetchAndIndex fetches a URL, converts to markdown, and indexes the content.
func (o *Optimizer) FetchAndIndex(ctx context.Context, url string, source string) (*FetchResult, error) {
	result, err := o.fetcher.FetchAndConvert(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	if _, err := o.indexer.IndexContent(ctx, result.Markdown, source, url, "", "prose"); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}

	return result, nil
}

// ExecuteBatch runs multiple commands sequentially, indexes output, and
// optionally searches the indexed content.
func (o *Optimizer) ExecuteBatch(ctx context.Context, commands []BatchCommand, queries []string, intent string) (*BatchResult, error) {
	return o.batch.ExecuteBatch(ctx, commands, queries, "", intent)
}

// Store exposes the underlying Store for session event operations.
func (o *Optimizer) Store() *Store {
	return o.store
}

// Close stops the background evictor. Safe to call multiple times.
func (o *Optimizer) Close() {
	o.evictor.Close()
}
