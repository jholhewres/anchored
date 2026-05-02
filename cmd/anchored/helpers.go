package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/importer"
	"github.com/jholhewres/anchored/pkg/memory"
)

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func initService(configPath string) (*config.Config, *slog.Logger, *memory.Service, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}

	if err := config.EnsureDirs(cfg); err != nil {
		return nil, nil, nil, fmt.Errorf("ensure dirs: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	svc, err := memory.NewService(cfg, logger)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("init service: %w", err)
	}

	return cfg, logger, svc, nil
}

type serviceStoreAdapter struct {
	svc *memory.Service
}

func (a *serviceStoreAdapter) SaveRaw(ctx context.Context, content, category, source string, cwd string) error {
	return a.svc.SaveRawNoEmbed(ctx, content, category, source, cwd)
}

func (a *serviceStoreAdapter) SaveRawWithSource(ctx context.Context, content, category, source string, sourceID *string, cwd string) error {
	_, err := a.svc.SaveWithOptions(ctx, memory.SaveOptions{
		Content:   content,
		Category:  category,
		Source:    source,
		SourceID:  sourceID,
		CWD:       cwd,
		SkipEmbed: true,
	})
	return err
}

func (a *serviceStoreAdapter) CreateImport(id, source, path string) error {
	_, err := a.svc.StoreDB().Exec(
		`INSERT INTO imports (id, source, path, status, started_at) VALUES (?, ?, ?, 'running', CURRENT_TIMESTAMP)`,
		id, source, path,
	)
	return err
}

func (a *serviceStoreAdapter) UpdateImport(id, status string, memoriesImported int, errMsg string) error {
	_, err := a.svc.StoreDB().Exec(
		`UPDATE imports SET status = ?, memories_imported = ?, finished_at = CURRENT_TIMESTAMP, error = ? WHERE id = ?`,
		status, memoriesImported, errMsg, id,
	)
	return err
}

func (a *serviceStoreAdapter) GetLastImport(source string) (*importer.ImportRecordInfo, error) {
	row := a.svc.StoreDB().QueryRow(
		`SELECT source, path, status, finished_at FROM imports WHERE source = ? ORDER BY started_at DESC LIMIT 1`, source,
	)
	var r importer.ImportRecordInfo
	var finishedAt sql.NullTime
	err := row.Scan(&r.Source, &r.Path, &r.Status, &finishedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return &r, nil
}
