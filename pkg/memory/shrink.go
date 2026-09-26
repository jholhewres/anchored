package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jholhewres/anchored/pkg/config"
)

// ErrDatabaseBusy reports that another process still holds the database, so the
// file cannot be replaced.
var ErrDatabaseBusy = errors.New("another process is using the database")

// ShrinkStats reports the file sizes either side of a rewrite.
type ShrinkStats struct {
	SizeBefore int64
	SizeAfter  int64
}

// Shrink rewrites the database into a fresh file holding only live pages, then
// swaps it into place.
//
// It deliberately does NOT use plain `VACUUM`. In-place VACUUM builds its copy
// in a temporary file beside the original and therefore needs free space of
// roughly twice the current size — the exact thing a caller reaching for this
// command does not have, since the reason to run it is a database that grew
// past its disk. `VACUUM INTO` writes only the live pages, so the requirement
// drops to the FINAL size, which here is a fraction of the original.
//
// The swap replaces a file other processes may hold open: their writes would
// land in an unlinked inode and vanish. So Shrink takes the database to itself
// first, through an exclusive lock, and refuses with ErrDatabaseBusy rather
// than silently losing anyone's writes. The caller must have closed its own
// Service beforehand — its connections count as holders like any other.
func Shrink(ctx context.Context, dbPath string) (ShrinkStats, error) {
	var stats ShrinkStats

	before, err := fileSize(dbPath)
	if err != nil {
		return stats, err
	}
	stats.SizeBefore = before
	stats.SizeAfter = before

	db, err := openExclusive(ctx, dbPath)
	if err != nil {
		return stats, err
	}
	defer db.Close()

	wantMemories, err := countScalar(ctx, db, `SELECT COUNT(*) FROM memories`)
	if err != nil {
		return stats, err
	}
	wantRevisions, err := countScalar(ctx, db, `SELECT COUNT(*) FROM memory_revisions`)
	if err != nil {
		return stats, err
	}

	tmp := dbPath + ".shrink-tmp"
	_ = os.Remove(tmp)
	// VACUUM INTO refuses to overwrite, so the stale temp above must be gone.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		_ = os.Remove(tmp)
		return stats, fmt.Errorf("vacuum into: %w", err)
	}

	// Never replace a live database with a file that has not been read back.
	if err := verifyShrunkCopy(ctx, tmp, wantMemories, wantRevisions); err != nil {
		_ = os.Remove(tmp)
		return stats, err
	}
	// VACUUM INTO creates the copy with the default 0644; it becomes the
	// database, which must stay owner-only.
	if err := os.Chmod(tmp, config.PrivateFileMode); err != nil && runtime.GOOS != "windows" {
		_ = os.Remove(tmp)
		return stats, fmt.Errorf("restrict compacted copy: %w", err)
	}

	// Close before swapping: on Windows the open handle would block the rename,
	// and everywhere else it would leave this connection pointing at the file
	// that is about to be replaced.
	if err := db.Close(); err != nil {
		_ = os.Remove(tmp)
		return stats, fmt.Errorf("close before swap: %w", err)
	}

	if err := os.Rename(tmp, dbPath); err != nil {
		_ = os.Remove(tmp)
		return stats, fmt.Errorf("swap compacted database: %w", err)
	}
	// The old WAL and shared-memory files describe the file that was just
	// replaced. Leaving them behind would have SQLite recover a journal onto a
	// database it was never written for.
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	after, err := fileSize(dbPath)
	if err != nil {
		return stats, err
	}
	stats.SizeAfter = after
	return stats, nil
}

// openExclusive returns a connection that holds the database alone, or
// ErrDatabaseBusy. In WAL mode a reader does not block a writer, so holding a
// write transaction is not evidence of exclusivity; locking_mode=exclusive is,
// because taking it fails while any other connection has the shared-memory
// index mapped. The write below is what forces the lock to actually be taken —
// SQLite defers it until the first access.
func openExclusive(ctx context.Context, dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("%s?_busy_timeout=2000&_locking_mode=exclusive&_txlock=immediate", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	// One connection only: a pool would race itself for the exclusive lock.
	db.SetMaxOpenConns(1)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %v", ErrDatabaseBusy, err)
	}
	if err := tx.Rollback(); err != nil {
		db.Close()
		return nil, fmt.Errorf("release exclusivity probe: %w", err)
	}
	return db, nil
}

// verifyShrunkCopy opens the rewritten file and checks it is sound and complete
// before anything irreversible happens to the original.
func verifyShrunkCopy(ctx context.Context, path string, wantMemories, wantRevisions int64) error {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return fmt.Errorf("open compacted copy: %w", err)
	}
	defer db.Close()

	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil {
		return fmt.Errorf("integrity check on compacted copy: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("compacted copy failed integrity check: %s", check)
	}

	gotMemories, err := countScalar(ctx, db, `SELECT COUNT(*) FROM memories`)
	if err != nil {
		return err
	}
	gotRevisions, err := countScalar(ctx, db, `SELECT COUNT(*) FROM memory_revisions`)
	if err != nil {
		return err
	}
	if gotMemories != wantMemories || gotRevisions != wantRevisions {
		return fmt.Errorf(
			"compacted copy is incomplete: memories %d/%d, revisions %d/%d",
			gotMemories, wantMemories, gotRevisions, wantRevisions)
	}
	return nil
}

func countScalar(ctx context.Context, db *sql.DB, query string) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("count rows: %w", err)
	}
	return n, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(filepath.Clean(path))
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return info.Size(), nil
}
