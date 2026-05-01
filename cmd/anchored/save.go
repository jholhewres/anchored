package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func runSave(args []string) {
	fs := newFlagSet("save")
	category := fs.String("category", "fact", "memory category")
	project := fs.String("project", "", "project ID")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	content := strings.Join(fs.Args(), " ")
	if content == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored save <content> [--category] [--project]")
		os.Exit(1)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	m, err := svc.Save(ctx, content, *category, "cli", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "save error: %v\n", err)
		os.Exit(1)
	}

	_ = project
	fmt.Printf("Saved memory %s [%s]\n", m.ID, m.Category)
}
