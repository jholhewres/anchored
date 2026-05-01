package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runList(args []string) {
	fs := newFlagSet("list")
	category := fs.String("category", "", "filter by category")
	limit := fs.Int("limit", 20, "max results")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	opts := memory.ListOptions{
		Limit:    *limit,
		Category: *category,
	}

	memories, err := svc.List(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list error: %v\n", err)
		os.Exit(1)
	}

	if len(memories) == 0 {
		fmt.Println("No memories found.")
		return
	}

	for _, m := range memories {
		proj := ""
		if m.ProjectID != nil {
			proj = *m.ProjectID
		}
		fmt.Printf("%s [%s] %s%s\n", m.ID, m.Category, truncate(m.Content, 100), projDisplay(proj))
	}
}

func projDisplay(p string) string {
	if p == "" {
		return ""
	}
	return " (" + p + ")"
}
