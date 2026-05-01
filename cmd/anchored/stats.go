package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/stack"
)

func runStats(args []string) {
	fs := newFlagSet("stats")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	cfg, logger, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	stats, err := svc.Stats(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stats error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Total memories: %d\n", stats.TotalMemories)

	if len(stats.ByCategory) > 0 {
		fmt.Println("\nBy category:")
		for cat, count := range stats.ByCategory {
			fmt.Printf("  %s: %d\n", cat, count)
		}
	}

	if len(stats.ByProject) > 0 {
		fmt.Println("\nBy project:")
		for proj, count := range stats.ByProject {
			fmt.Printf("  %s: %d\n", proj, count)
		}
	}

	fmt.Println("\nStack metrics:")
	metrics := computeStackMetrics(svc.StoreDB(), cfg, logger)
	fmt.Printf("  l0_bytes=%d l1_bytes=%d l2_bytes=%d renders=%d\n",
		metrics.LayerBytesL0, metrics.LayerBytesL1, metrics.LayerBytesL2, metrics.TotalRenders)
	fmt.Printf("  l1_cache_hits=%d l1_cache_misses=%d\n",
		metrics.L1CacheHits, metrics.L1CacheMisses)
}

func computeStackMetrics(db *sql.DB, cfg *config.Config, logger *slog.Logger) stack.StackMetrics {
	home, _ := os.UserHomeDir()
	identityPath := home + "/.anchored/identity.md"

	identityLayer := stack.NewIdentityLayer(identityPath, logger, 800)
	projectLayer := stack.NewProjectLayer(func() string { return "" })
	onDemandLayer := stack.NewOnDemandLayer(
		&dbAccessor{db: db},
		memory.NewEntityDetector(db, memory.DefaultEntityDetectorConfig(), logger),
		memory.NewTopicChangeDetector(nil, nil),
		logger,
		stack.OnDemandLayerConfig{},
	)

	s := stack.NewStack(identityLayer, projectLayer, onDemandLayer, cfg.Stack.BudgetBytes, logger)
	s.Render()
	return s.Metrics()
}
