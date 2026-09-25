package memory

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Several anchored processes (one per open editor session, plus hooks and
// the maintenance timer) hold the same database open. When a new binary brings
// migrations, all of them race to apply them on their next open; each
// migration must run exactly once and none of the losers may fail to start.
func TestMigrate_ConcurrentOpenersApplyEachMigrationOnce(t *testing.T) {
	all := schemaMigrations()
	// Pending migrations that would fail if run twice: an ALTER TABLE, and a
	// plain CREATE TABLE without IF NOT EXISTS.
	newOnes := []migration{
		{Name: "900_race_alter", Up: `ALTER TABLE memories ADD COLUMN race_probe TEXT`},
		{Name: "901_race_table", Up: `CREATE TABLE race_probe_table (id INTEGER PRIMARY KEY)`},
		{Name: "902_race_index", Up: `CREATE INDEX idx_race_probe ON memories(race_probe)`},
	}

	for round := 0; round < 5; round++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("race-%d.db", round))
		dsn := path + "?_journal_mode=WAL&_busy_timeout=30000&_txlock=immediate&_foreign_keys=on"

		// The database already exists at the previous version.
		seed, err := sql.Open("sqlite3", dsn)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrate(seed, all); err != nil {
			t.Fatal(err)
		}
		seed.Close()

		const openers = 6
		dbs := make([]*sql.DB, openers)
		for i := range dbs {
			db, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			if err := db.Ping(); err != nil {
				t.Fatal(err)
			}
			dbs[i] = db
		}

		upgraded := append(append([]migration{}, all...), newOnes...)
		start := make(chan struct{})
		errs := make(chan error, openers)
		var wg sync.WaitGroup
		for _, db := range dbs {
			wg.Add(1)
			go func(db *sql.DB) {
				defer wg.Done()
				<-start
				errs <- migrate(db, upgraded)
			}(db)
		}
		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: a concurrent opener failed to migrate: %v", round, err)
			}
		}

		var recorded int
		if err := dbs[0].QueryRow(
			"SELECT COUNT(*) FROM migrations WHERE name LIKE '90%'",
		).Scan(&recorded); err != nil {
			t.Fatal(err)
		}
		if recorded != len(newOnes) {
			t.Errorf("round %d: expected %d new migrations recorded once each, got %d", round, len(newOnes), recorded)
		}
	}
}

// Opening an up-to-date database must not wait for the write lock: every
// serve, CLI and dashboard start migrates, and a long writer (import,
// compact, an embedding activation) would otherwise stall or fail them.
func TestMigrate_UpToDateDatabaseTakesNoWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uptodate.db")
	seed, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=30000")
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	if err := Migrate(seed); err != nil {
		t.Fatal(err)
	}

	writer, err := seed.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer writer.ExecContext(context.Background(), "ROLLBACK")

	opener, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=200&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer opener.Close()
	start := time.Now()
	if err := Migrate(opener); err != nil {
		t.Fatalf("migrating an up-to-date database while another process writes: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("up-to-date migrate waited %v for the write lock", elapsed)
	}
}
