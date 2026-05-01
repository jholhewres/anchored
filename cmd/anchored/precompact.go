package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runPrecompact(args []string) {
	fs := newFlagSet("precompact")
	cwd := fs.String("cwd", "", "current working directory for project detection")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		os.Exit(1)
	}

	text := strings.TrimSpace(string(content))
	if text == "" {
		fmt.Println("No content to capture.")
		return
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	cwdVal := *cwd
	if cwdVal == "" {
		cwdVal = "."
	}

	m, err := svc.SaveWithOptions(context.Background(), memory.SaveOptions{
		Content:  text,
		Category: "",
		Source:   "precompact",
		CWD:      cwdVal,
	})
	if err != nil {
		slog.Error("failed to save precompact memory", "error", err)
		os.Exit(1)
	}

	fmt.Printf("Pre-compact context saved [%s] memory %s (%d bytes)\n", m.Category, m.ID, len(text))
}
