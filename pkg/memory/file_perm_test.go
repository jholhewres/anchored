//go:build !windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The database holds every memory, secrets that slipped past redaction
// included. A default umask made it world-readable (0644); the store keeps it
// and its WAL/SHM owner-only, tightening older files on open.
func TestNewSQLiteStore_KeepsDatabaseFilesPrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	path := filepath.Join(t.TempDir(), "private.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveTemporal(context.Background(), Memory{ID: "m1", Category: "fact", Content: "private"}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), info.Mode().Perm())
		}
	}
	_ = store.Close()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("reopening must tighten an existing 644 database, got %o", info.Mode().Perm())
	}
}

// Upgrading while a 0.19 process still runs: its WAL and SHM already exist
// with the old 0644 and must be tightened too, not only new ones.
func TestNewSQLiteStore_TightensAnExistingWALAndSHM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p+".touch", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(p + ".touch")
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := KeepDatabasePrivate(path); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), info.Mode().Perm())
		}
	}
}

// A database_path mistakenly pointing at a directory must not have the
// directory chmod-ed to 0600 (it would lose its traverse bit).
func TestKeepDatabasePrivate_LeavesDirectoriesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = KeepDatabasePrivate(dir)
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o755 {
		t.Errorf("directory mode changed to %o", info.Mode().Perm())
	}
}

// VACUUM INTO writes its copy with the default mode; the swap must not turn a
// private database into a world-readable one until the next open.
func TestShrinkKeepsTheDatabasePrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	path := shrinkFixture(t, 50)
	if _, err := Shrink(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("shrunk database mode = %o, want 600", info.Mode().Perm())
	}
}
