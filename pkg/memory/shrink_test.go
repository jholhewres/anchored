package memory

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// shrinkFixture builds a database with enough redundant history that a rewrite
// has something to reclaim, and returns its path with every handle closed.
func shrinkFixture(t *testing.T, extra int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shrink.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedRedundantHistory(t, store, extra)
	if _, err := Compact(context.Background(), store.DB(), CompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShrinkRewritesAndPreservesData(t *testing.T) {
	ctx := context.Background()
	path := shrinkFixture(t, 400)

	before := readCounts(t, path)

	stats, err := Shrink(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SizeAfter >= stats.SizeBefore {
		t.Fatalf("file did not shrink: %d -> %d", stats.SizeBefore, stats.SizeAfter)
	}

	after := readCounts(t, path)
	if after != before {
		t.Fatalf("row counts changed across shrink: %+v -> %+v", before, after)
	}

	// The journal files of the replaced database must not survive it.
	for _, suffix := range []string{"-wal", "-shm", ".shrink-tmp"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("%s survived the swap", suffix)
		}
	}

	// And the result is a working database, not just a well-formed file.
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.List(ctx, ListOptions{Limit: 1}); err != nil {
		t.Fatalf("shrunk database is not usable: %v", err)
	}
}

// The swap replaces a file other processes hold open; their writes would land
// in an unlinked inode. Shrink must refuse instead.
func TestShrinkRefusesWhileAnotherConnectionHoldsTheDatabase(t *testing.T) {
	path := shrinkFixture(t, 20)

	holder, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=1000")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	// Force the connection to actually attach to the shared-memory index.
	if _, err := holder.Exec(`SELECT COUNT(*) FROM memories`); err != nil {
		t.Fatal(err)
	}

	sizeBefore := statSize(t, path)
	_, err = Shrink(context.Background(), path)
	if !errors.Is(err, ErrDatabaseBusy) {
		t.Fatalf("Shrink error = %v, want ErrDatabaseBusy", err)
	}
	if statSize(t, path) != sizeBefore {
		t.Fatal("the database was touched despite the refusal")
	}
	if _, err := os.Stat(path + ".shrink-tmp"); !os.IsNotExist(err) {
		t.Fatal("a temp file was left behind by the refusal")
	}
}

// A copy that does not read back complete must never replace the original.
func TestVerifyShrunkCopyRejectsIncompleteCopy(t *testing.T) {
	ctx := context.Background()
	path := shrinkFixture(t, 10)
	counts := readCounts(t, path)

	if err := verifyShrunkCopy(ctx, path, counts.memories+1, counts.revisions); err == nil {
		t.Fatal("a copy missing memories must be rejected")
	}
	if err := verifyShrunkCopy(ctx, path, counts.memories, counts.revisions+1); err == nil {
		t.Fatal("a copy missing revisions must be rejected")
	}
	if err := verifyShrunkCopy(ctx, path, counts.memories, counts.revisions); err != nil {
		t.Fatalf("a complete copy must be accepted: %v", err)
	}
}

func TestShrinkIsSafeToRepeat(t *testing.T) {
	ctx := context.Background()
	path := shrinkFixture(t, 50)

	first, err := Shrink(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	counts := readCounts(t, path)

	second, err := Shrink(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if second.SizeAfter > first.SizeAfter {
		t.Fatalf("a second shrink grew the file: %d -> %d", first.SizeAfter, second.SizeAfter)
	}
	if readCounts(t, path) != counts {
		t.Fatal("a second shrink changed the data")
	}
}

type dbCounts struct{ memories, revisions, vectors int64 }

func readCounts(t *testing.T, path string) dbCounts {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var c dbCounts
	if c.memories, err = countScalar(ctx, db, `SELECT COUNT(*) FROM memories`); err != nil {
		t.Fatal(err)
	}
	if c.revisions, err = countScalar(ctx, db, `SELECT COUNT(*) FROM memory_revisions`); err != nil {
		t.Fatal(err)
	}
	if c.vectors, err = countScalar(ctx, db, `SELECT COUNT(*) FROM memory_embedding_vectors`); err != nil {
		t.Fatal(err)
	}
	return c
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	n, err := fileSize(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
