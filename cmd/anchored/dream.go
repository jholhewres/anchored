package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jholhewres/anchored/pkg/dream"
)

func runDream(args []string) {
	fs := newFlagSet("dream")
	dryRun := fs.Bool("dry-run", true, "analyze only, do not apply changes")
	aggressiveness := fs.String("aggressiveness", "moderate", "conservative, moderate, or aggressive")
	maxDeletions := fs.Int("max-deletions", 50, "maximum soft-deletions per run")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	_, logger, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	db := svc.StoreDB()
	ctx := context.Background()

	dreamCfg := dream.DreamConfigForAggressiveness(*aggressiveness)
	dreamCfg.MaxDeletionsPerRun = *maxDeletions

	analyzeCfg := dream.DefaultDreamConfig()
	analyzeCfg.DedupThreshold = dreamCfg.DedupThreshold

	analyzer := dream.NewAnalyzer(db, svc.VectorCache(), analyzeCfg, logger)

	report, err := analyzer.Analyze(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream analysis failed: %v\n", err)
		os.Exit(1)
	}

	runID := fmt.Sprintf("dream-%d", time.Now().Unix())
	dream.SaveDreamRun(ctx, db, runID, report, map[bool]string{true: "dry_run", false: "completed"}[*dryRun])
	dream.SaveDreamActions(ctx, db, runID, report.Actions)

	fmt.Println("Dream Analysis Report")
	fmt.Println("=====================")
	fmt.Printf("Total memories analyzed: %d\n", report.TotalMemories)
	fmt.Printf("Exact duplicates: %d\n", report.ExactDupes)
	fmt.Printf("Near-duplicates: %d\n", report.NearDupes)
	fmt.Printf("Total actions proposed: %d\n", len(report.Actions))
	fmt.Println()

	if len(report.Actions) > 0 {
		fmt.Println("Proposed Actions:")
		for _, a := range report.Actions {
			switch a.ActionType {
			case "dedup":
				fmt.Printf("  DEDUP    [%s] = [%s] (confidence: %.2f, %s)\n", a.MemoryID, a.RelatedMemoryID, a.Confidence, a.Reason)
			case "contradiction":
				fmt.Printf("  CONTRADICT [%s] vs [%s] (confidence: %.2f, %s)\n", a.MemoryID, a.RelatedMemoryID, a.Confidence, a.Reason)
			}
		}
	}

	if !*dryRun {
		consolidator := dream.NewConsolidator(db, logger)
		result, err := consolidator.Consolidate(ctx, report, dreamCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "consolidation failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		fmt.Println("Consolidation Results:")
		fmt.Printf("  Soft-deleted: %d\n", result.SoftDeleted)
		fmt.Printf("  Flagged: %d\n", result.Flagged)
		fmt.Printf("  Skipped: %d\n", result.Skipped)
	} else {
		fmt.Println("\n(dry-run mode — no changes applied)")
	}
}
