package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

func runUpdate(args []string) {
	fs := newFlagSet("update")
	configPath := fs.String("config", "", "path to config file")
	content := fs.String("content", "", "new content")
	category := fs.String("category", "", "new category")
	fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored update <id> --content <content> [--category <category>]")
		os.Exit(1)
	}
	if *content == "" && *category == "" {
		fmt.Fprintln(os.Stderr, "Error: must provide --content or --category")
		os.Exit(1)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	m, err := svc.Update(context.Background(), id, *content, *category)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated [%s] memory %s\n", m.Category, m.ID)
}
