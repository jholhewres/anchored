//go:build windows

package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jholhewres/anchored/pkg/config"
)

type noopOptimizer struct{}

func NewCtxOptimizer(db *sql.DB, cfg config.ContextOptimizerConfig, logger *slog.Logger) (OptimizerFacade, error) {
	return &noopOptimizer{}, nil
}

func (n *noopOptimizer) Execute(ctx context.Context, code string, language string, timeoutMs int) (string, string, int, string, bool, bool, error) {
	return "", "", 0, "", false, false, fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) ExecuteFile(ctx context.Context, path string, language string, code string, timeoutMs int) (string, string, int, string, bool, bool, error) {
	return "", "", 0, "", false, false, fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) IndexContent(ctx context.Context, content string, source string, label string) (string, error) {
	return "", fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) IndexRaw(ctx context.Context, content string, source string, label string) (string, error) {
	return "", fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) Search(ctx context.Context, query string, maxResults int, contentType string, source string) ([]OptimizerSearchResult, error) {
	return nil, fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) FetchAndIndex(ctx context.Context, url string, source string) (string, string, bool, error) {
	return "", "", false, fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) ExecuteBatch(ctx context.Context, commands []OptimizerBatchCommand, queries []string, intent string) (*OptimizerBatchResult, error) {
	return nil, fmt.Errorf("not supported on windows")
}

func (n *noopOptimizer) Close() {}
