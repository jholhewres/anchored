package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

func runForget(args []string) {
	fs := newFlagSet("forget")
	configPath := fs.String("config", "", "path to config file")
	hard := fs.Bool("hard", false, "permanently delete (default: soft delete)")
	fs.Parse(args)

	id := fs.Arg(0)
	if id == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored forget <id> [--hard]")
		os.Exit(1)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	if *hard {
		if err := svc.Forget(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Permanently deleted memory %s\n", id)
	} else {
		if err := svc.SoftForget(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Soft-deleted memory %s\n", id)
	}
}
