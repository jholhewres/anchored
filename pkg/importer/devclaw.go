package importer

import (
	"context"
	"database/sql"

	_ "github.com/mattn/go-sqlite3"
)

type DevClawImporter struct {
	dbPath string
	log    func(string, ...any)
}

func NewDevClawImporter(dbPath string, log func(string, ...any)) *DevClawImporter {
	return &DevClawImporter{dbPath: dbPath, log: log}
}

func (i *DevClawImporter) Name() string { return "devclaw" }
func (i *DevClawImporter) Path() string { return i.dbPath }

func (i *DevClawImporter) Detect() bool {
	db, err := sql.Open("sqlite3", i.dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chunks'").Scan(&n)
	return err == nil && n > 0
}

func (i *DevClawImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	result := ImportResult{Source: i.Name()}

	db, err := sql.Open("sqlite3", i.dbPath)
	if err != nil {
		result.Errors++
		return result
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT text FROM chunks ORDER BY file_id, chunk_idx")
	if err != nil {
		result.Errors++
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			result.Errors++
			continue
		}
		result.Found++
		if err := store.SaveRaw(ctx, text, "fact", "devclaw", ""); err != nil {
			result.Errors++
			continue
		}
		result.Imported++
	}

	return result
}


