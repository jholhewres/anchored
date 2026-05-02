package memory

import (
	"database/sql"
	"fmt"

	ctxpkg "github.com/jholhewres/anchored/pkg/context"
)

type migration struct {
	Name string
	Up   string
}

func Migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	migrations := []migration{
		{Name: "001_initial_schema", Up: initSchema()},
		{Name: "002_indexed_files", Up: `CREATE TABLE IF NOT EXISTS indexed_files (
			path TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL,
			indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`},
		{Name: "003_content_hash_soft_delete", Up: `
			ALTER TABLE memories ADD COLUMN content_hash TEXT;
			ALTER TABLE memories ADD COLUMN deleted_at DATETIME;
			UPDATE memories SET content_hash = '' WHERE content_hash IS NULL;
			CREATE INDEX IF NOT EXISTS idx_memories_content_hash ON memories(content_hash, project_id);
			CREATE INDEX IF NOT EXISTS idx_memories_deleted_at ON memories(deleted_at);
		`},
		{Name: "004_cross_project_search", Up: `
			CREATE INDEX IF NOT EXISTS idx_memories_global_search ON memories(category, deleted_at);
			CREATE INDEX IF NOT EXISTS idx_memories_source ON memories(source);
		`},
		{Name: "005_live_sessions", Up: `
			ALTER TABLE sessions ADD COLUMN last_activity_at DATETIME;
			ALTER TABLE sessions ADD COLUMN ended_at DATETIME;
			ALTER TABLE sessions ADD COLUMN source_tool TEXT;
			ALTER TABLE sessions ADD COLUMN metadata TEXT;
			CREATE INDEX IF NOT EXISTS idx_sessions_source_session_id ON sessions(source_session_id);
			CREATE INDEX IF NOT EXISTS idx_sessions_last_activity ON sessions(last_activity_at);
		`},
		{Name: "006_auto_capture", Up: `
			CREATE INDEX IF NOT EXISTS idx_memories_source_type ON memories(source);
		`},
		{Name: "007_dream_system", Up: `
			CREATE TABLE IF NOT EXISTS dream_runs (
				id TEXT PRIMARY KEY,
				started_at DATETIME,
				finished_at DATETIME,
				config TEXT,
				memories_analyzed INTEGER DEFAULT 0,
				actions_proposed INTEGER DEFAULT 0,
				actions_applied INTEGER DEFAULT 0,
				status TEXT DEFAULT 'pending'
			);
			CREATE TABLE IF NOT EXISTS dream_actions (
				id TEXT PRIMARY KEY,
				run_id TEXT REFERENCES dream_runs(id),
				memory_id TEXT,
				related_memory_id TEXT,
				action_type TEXT,
				confidence REAL,
				reason TEXT,
				proposed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				applied_at DATETIME,
				status TEXT DEFAULT 'proposed'
			);
		`},
		{Name: "008_content_optimizer", Up: ctxpkg.MigrationSQL},
	}

	for _, m := range migrations {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM migrations WHERE name = ?", m.Name).Scan(&count)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.Name, err)
		}
		if count > 0 {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %s: %w", m.Name, err)
		}

		if _, err := tx.Exec(m.Up); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}

		if _, err := tx.Exec("INSERT INTO migrations (name) VALUES (?)", m.Name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
	}

	return nil
}
