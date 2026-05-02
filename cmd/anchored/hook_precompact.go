package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runHookPreCompact(args []string) {
	fs := newFlagSet("hook precompact")
	sessionID := fs.String("session-id", "", "session identifier")
	configPath := fs.String("config", "", "path to config file")
	cwd := fs.String("cwd", "", "current working directory")
	fs.Parse(args)

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		os.Exit(1)
	}

	text := strings.TrimSpace(string(content))
	if text == "" {
		outputJSON(map[string]any{"snapshot_saved": false, "error": "empty content"})
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

	ctx := context.Background()
	db := svc.StoreDB()

	metadata := truncate(text, 4096)
	eventID := newHookID()

	projectID := svc.ResolveProject(cwdVal)

	_, err = db.ExecContext(ctx,
		`INSERT INTO session_events (id, session_id, project_id, event_type, priority, summary, metadata, created_at)
		 VALUES (?, ?, ?, 'precompact_snapshot', 1, ?, ?, datetime('now'))`,
		eventID, *sessionID, projectID, truncate(text, 500), metadata,
	)
	if err != nil {
		slog.Warn("failed to save session event", "error", err)
	}

	m, err := svc.SaveWithOptions(ctx, memory.SaveOptions{
		Content:  text,
		Category: "summary",
		Source:   "precompact_hook",
		CWD:      cwdVal,
	})
	if err != nil {
		slog.Error("failed to save precompact memory", "error", err)
		os.Exit(1)
	}

	outputJSON(map[string]any{
		"snapshot_saved": true,
		"memory_id":      m.ID,
		"event_id":       eventID,
	})
}
