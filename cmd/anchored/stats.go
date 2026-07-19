package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
)

func runStats(args []string) {
	fs := newFlagSet("stats")
	configPath := fs.String("config", "", "path to config file")
	tokens := fs.Bool("tokens", false, "show context-token savings (injected vs. static baseline) over the last 7 days")
	fs.Parse(args)

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	if *tokens {
		printTokenStats(svc.StoreDB())
		return
	}

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

// printTokenStats renders the 7-day context-token telemetry: how many tokens
// anchored injected vs. what the static-context baseline would have cost.
func printTokenStats(db *sql.DB) {
	const days = 7
	s, err := queryRecallSummary(db, days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "token stats error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Context tokens — last %d days\n", days)
	if s.Injections == 0 {
		fmt.Println("  no recall telemetry yet (start a session or send a prompt with anchored hooks installed)")
		return
	}
	fmt.Printf("  Injections:       %d\n", s.Injections)
	fmt.Printf("  Injected tokens:  %d\n", s.InjectedTokens)
	fmt.Printf("  Baseline tokens:  %d  (static CLAUDE.md/AGENTS.md + skills)\n", s.BaselineTokens)
	fmt.Printf("  Savings:          %.0f%%\n", s.SavingsPct())
}
