package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

func runPurge(args []string) {
	fs := newFlagSet("purge")
	configPath := fs.String("config", "", "path to config file")
	hard := fs.Bool("hard", false, "wipe DB completely (default: soft-delete recoverable for 30 days)")
	yes := fs.Bool("yes", false, "skip confirmation prompt")
	fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	dbPath := cfg.Memory.DatabasePath
	if dbPath == "" {
		fmt.Fprintln(os.Stderr, "no database path in config")
		os.Exit(1)
	}

	if *hard {
		runHardPurge(dbPath, *yes)
		return
	}
	runSoftPurge(dbPath, *yes)
}

func runSoftPurge(dbPath string, yes bool) {
	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "database not found: %s\n", dbPath)
		os.Exit(1)
	}

	if !yes && !confirm(fmt.Sprintf("Soft-delete ALL memories in %s? (recoverable for 30 days via 'anchored dream')", dbPath)) {
		fmt.Println("aborted")
		return
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	res, err := db.Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE deleted_at IS NULL`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "soft-delete: %v\n", err)
		os.Exit(1)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("Soft-deleted %d memories. They remain in the DB until 'anchored dream --hard' clears them.\n", n)
}

func runHardPurge(dbPath string, yes bool) {
	if !yes && !confirm(fmt.Sprintf("HARD reset will delete EVERYTHING (memories, KG, sessions, imports). Backup will be made. Continue?")) {
		fmt.Println("aborted")
		return
	}

	if _, err := os.Stat(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "database not found: %s\n", dbPath)
		os.Exit(1)
	}

	stamp := time.Now().Format("2006-01-02-150405")
	backupPath := dbPath + "." + stamp + ".bak"

	// Flush the WAL into the main DB so the copy is self-contained even if
	// another process holds the DB open.
	if db, err := sql.Open("sqlite3", dbPath); err == nil {
		_, _ = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		db.Close()
	}

	if err := copyFile(dbPath, backupPath); err != nil {
		fmt.Fprintf(os.Stderr, "backup failed: %v\n", err)
		os.Exit(1)
	}

	// Also move the WAL/SHM siblings if they exist, so the new DB starts clean.
	for _, suffix := range []string{"-wal", "-shm"} {
		sib := dbPath + suffix
		if _, err := os.Stat(sib); err == nil {
			_ = os.Rename(sib, sib+"."+stamp+".bak")
		}
	}

	if err := os.Remove(dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "remove db: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Hard-purged. Backup at %s\n", backupPath)
	fmt.Println("Run 'anchored serve' (or any subcommand) to recreate an empty database.")
}

func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes"
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// The copy is a whole database: every memory, and whatever secrets
	// slipped past redaction. os.Create would make it 0644 minus the umask.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, config.PrivateFileMode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
