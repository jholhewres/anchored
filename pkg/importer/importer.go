package importer

import (
	"context"
	"log/slog"
)

type ImportResult struct {
	Source  string
	Found   int
	Imported int
	Skipped  int
	Errors   int
}

type Source interface {
	Name() string
	Detect() bool
	Import(ctx context.Context, store ImportStore) ImportResult
}

type ImportStore interface {
	SaveRaw(ctx context.Context, content, category, source string, cwd string) error
}

func RunAll(ctx context.Context, sources []Source, store ImportStore, logger *slog.Logger) []ImportResult {
	var results []ImportResult
	for _, src := range sources {
		if !src.Detect() {
			logger.Debug("source not detected, skipping", "source", src.Name())
			continue
		}
		logger.Info("importing from source", "source", src.Name())
		result := src.Import(ctx, store)
		results = append(results, result)
		logger.Info("import complete",
			"source", result.Source,
			"found", result.Found,
			"imported", result.Imported,
			"skipped", result.Skipped,
			"errors", result.Errors,
		)
	}
	return results
}
