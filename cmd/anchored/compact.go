package main

import (
	"context"
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
func runCompact(args []string) {
	fs := newFlagSet("compact")
	configPath := fs.String("config", "", "path to config file")
	dryRun := fs.Bool("dry-run", false, "report what would be removed, remove nothing")
	keepHistory := fs.Bool("keep-history", false, "prune only derived data, keep every revision")
	noVacuum := fs.Bool("no-vacuum", false, "skip the VACUUM that returns freed pages to the filesystem")
	fs.Parse(args)

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	stats, err := memory.Compact(context.Background(), svc.StoreDB(), memory.CompactOptions{
		KeepHistory: *keepHistory,
		DryRun:      *dryRun,
		Vacuum:      !*noVacuum && !*dryRun,
	})
	if err != nil {
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
		return
	}
	fmt.Printf("  database size         %s -> %s\n",
		humanBytes(stats.SizeBefore), humanBytes(stats.SizeAfter))
	if *noVacuum && stats.SizeAfter >= stats.SizeBefore {
		fmt.Println("  (pages were freed inside the file; run without --no-vacuum to shrink it)")
	}
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
