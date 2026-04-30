package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

type OpenCodeImporter struct {
	baseDir string
	log     func(string, ...any)
}

func NewOpenCodeImporter(baseDir string, log func(string, ...any)) *OpenCodeImporter {
	return &OpenCodeImporter{baseDir: baseDir, log: log}
}

func (i *OpenCodeImporter) Name() string { return "opencode" }

func (i *OpenCodeImporter) Detect() bool {
	patterns := []string{
		filepath.Join(i.baseDir, "opencode.json"),
		filepath.Join(i.baseDir, "sessions"),
	}
	for _, p := range patterns {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func (i *OpenCodeImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	return ImportResult{Source: i.Name(), Found: 0, Imported: 0}
}

type CursorImporter struct {
	baseDir string
	log     func(string, ...any)
}

func NewCursorImporter(baseDir string, log func(string, ...any)) *CursorImporter {
	return &CursorImporter{baseDir: baseDir, log: log}
}

func (i *CursorImporter) Name() string { return "cursor" }

func (i *CursorImporter) Detect() bool {
	rulesDir := filepath.Join(i.baseDir, "rules")
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".mdc") {
			return true
		}
	}
	return false
}

func (i *CursorImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	return ImportResult{Source: i.Name(), Found: 0, Imported: 0}
}
