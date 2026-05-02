package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

func runStats(args []string) {
	fs := newFlagSet("stats")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	_, _, svc, err := initService(*configPath)
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
}
