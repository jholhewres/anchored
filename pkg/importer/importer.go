package importer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"time"
)

type ImportResult struct {
	Source   string
	Found    int
	Imported int
	Skipped  int
	Errors   int
}

type Source interface {
	Name() string
	Path() string
	Detect() bool
	Import(ctx context.Context, store ImportStore) ImportResult
}

type ImportStore interface {
	SaveRaw(ctx context.Context, content, category, source string, cwd string) error
	SaveRawWithSource(ctx context.Context, content, category, source string, sourceID *string, cwd string) error
}

type ImportTracker interface {
	CreateImport(id, source, path string) error
	UpdateImport(id, status string, memoriesImported int, errMsg string) error
	GetLastImport(source string) (*ImportRecordInfo, error)
}

type ImportRecordInfo struct {
	Source    string
	Path      string
	Status    string
	FinishedAt *time.Time
}

type RunAllOptions struct {
	Force bool
}

func RunAll(ctx context.Context, sources []Source, store ImportStore, logger *slog.Logger, opts ...RunAllOptions) []ImportResult {
	var opt RunAllOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	tracker, _ := store.(ImportTracker)

	var results []ImportResult
	for _, src := range sources {
		if !src.Detect() {
			logger.Debug("source not detected, skipping", "source", src.Name())
			continue
		}

		sourceName := src.Name()
		sourcePath := src.Path()

		if tracker != nil && !opt.Force {
			if last, err := tracker.GetLastImport(sourceName); err != nil {
				logger.Warn("failed to check last import, proceeding", "source", sourceName, "error", err)
			} else if last != nil && last.Status == "done" && last.FinishedAt != nil {
				if info, err := os.Stat(sourcePath); err == nil {
					if !info.ModTime().After(*last.FinishedAt) {
						logger.Info("skipping unchanged source", "source", sourceName, "path", sourcePath, "last_import", last.FinishedAt.Format(time.RFC3339))
						results = append(results, ImportResult{Source: sourceName})
						continue
					}
				}
			}
		}

		var importID string
		if tracker != nil {
			importID = newImportID()
			if err := tracker.CreateImport(importID, sourceName, sourcePath); err != nil {
				logger.Warn("failed to create import record", "source", sourceName, "error", err)
				importID = ""
			}
		}

		logger.Info("importing from source", "source", sourceName)
		result := src.Import(ctx, store)
		results = append(results, result)

		if tracker != nil && importID != "" {
			status := "done"
			errMsg := ""
			if result.Errors > 0 {
				status = "done"
			}
			if err := tracker.UpdateImport(importID, status, result.Imported, errMsg); err != nil {
				logger.Warn("failed to update import record", "source", sourceName, "error", err)
			}
		}

		logger.Info("import complete",
			"source", result.Source,
			"found", result.Found,
			"imported", result.Imported,
			"skipped", result.Skipped,
			"errors", result.Errors,
		)
	}

	totalImported := 0
	for _, r := range results {
		totalImported += r.Imported
	}
	if totalImported > 0 {
		logger.Info("waiting for background embeddings to complete...", "imported", totalImported)
		time.Sleep(5 * time.Second)
	}

	return results
}

func newImportID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
