package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/jholhewres/anchored/pkg/memory"
)

// anchored compact reclaims the space taken by rows that carry no information:
// revisions repeating a state already recorded, the verbatim embedding copies
// those revisions dragged along, and completed job rows for revisions that are
// gone. It is the cleanup half of the no-op-save fix — the save path stopped
// producing these, this removes the ones already on disk.
//
// The sweep leaves freed pages inside the file for SQLite to reuse. Returning
// them to the filesystem means rewriting the file, which replaces it under
// every process holding it open, so --shrink is a separate opt-in that refuses
// to run unless it can take the database to itself.
func runCompact(args []string) {
	fs := newFlagSet("compact")
	configPath := fs.String("config", "", "path to config file")
	dryRun := fs.Bool("dry-run", false, "report what would be removed, remove nothing")
	keepHistory := fs.Bool("keep-history", false, "prune only derived data, keep every revision")
	shrink := fs.Bool("shrink", false, "also rewrite the file so freed pages return to the filesystem")
	fs.Parse(args)

	cfg, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	stats, err := memory.Compact(ctx, svc.StoreDB(), memory.CompactOptions{
		KeepHistory: *keepHistory,
		DryRun:      *dryRun,
	})
	if err != nil {
		svc.Close()
		slog.Error("compact failed", "error", err)
		os.Exit(1)
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Printf("Compact [%s]:\n", verb)
	fmt.Printf("  redundant revisions   %d\n", stats.RedundantRevisions)
	fmt.Printf("  superseded vectors    %d\n", stats.SupersededVectors)
	fmt.Printf("  completed jobs        %d\n", stats.CompletedJobs)

	if *dryRun {
		fmt.Printf("  database size         %s (unchanged)\n", humanBytes(stats.SizeBefore))
		svc.Close()
		return
	}

	if !*shrink {
		fmt.Printf("  database size         %s (freed pages stay available for reuse)\n",
			humanBytes(stats.SizeAfter))
		fmt.Println("  run with --shrink to return them to the filesystem")
		svc.Close()
		return
	}

	// Our own connections hold the database like anyone else's, so the service
	// has to be gone before the file can be taken exclusively.
	svc.Close()

	shrunk, err := memory.Shrink(ctx, cfg.Memory.DatabasePath)
	if err != nil {
		if errors.Is(err, memory.ErrDatabaseBusy) {
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Rows were removed, but the file was left alone:")
			fmt.Fprintln(os.Stderr, "another process still has the database open.")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "Rewriting it would replace the file those processes are")
			fmt.Fprintln(os.Stderr, "writing to and their writes would be lost. Close the editors")
			fmt.Fprintln(os.Stderr, "and agents connected to anchored, then run:")
			fmt.Fprintln(os.Stderr, "")
			fmt.Fprintln(os.Stderr, "  anchored compact --shrink")
			os.Exit(1)
		}
		slog.Error("shrink failed", "error", err)
		os.Exit(1)
	}

	fmt.Printf("  database size         %s -> %s\n",
		humanBytes(shrunk.SizeBefore), humanBytes(shrunk.SizeAfter))
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
