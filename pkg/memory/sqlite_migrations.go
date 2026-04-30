package memory

import (
	"database/sql"
	"fmt"
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
