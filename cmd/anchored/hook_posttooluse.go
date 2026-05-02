package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
)

func runHookPostToolUse(args []string) {
	fs := newFlagSet("hook posttooluse")
	sessionID := fs.String("session-id", "", "session identifier")
	configPath := fs.String("config", "", "path to config file")
	cwd := fs.String("cwd", "", "current working directory")
	fs.Parse(args)

	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		slog.Error("failed to read stdin", "error", err)
		os.Exit(1)
	}

	var input struct {
		Tool   string `json:"tool"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(content, &input); err != nil {
		outputJSON(map[string]any{"recorded": false, "error": "invalid JSON"})
		return
	}

	if *sessionID == "" {
		outputJSON(map[string]any{"recorded": false, "error": "session-id required"})
		return
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	db := svc.StoreDB()

	summary := input.Result
	if len(summary) > 500 {
		summary = summary[:500]
	}

	eventID := newHookID()
	cwdVal := *cwd
	if cwdVal == "" {
		cwdVal = "."
	}

	projectID := svc.ResolveProject(cwdVal)

	ctx := context.Background()
	_, err = db.ExecContext(ctx,
		`INSERT INTO session_events (id, session_id, project_id, event_type, priority, tool_name, summary, metadata, created_at)
		 VALUES (?, ?, 'tool_call', 3, ?, ?, ?, datetime('now'))`,
		eventID, *sessionID, projectID, input.Tool, summary, truncateJSONMetadata(cwdVal, string(content)),
	)
	if err != nil {
		outputJSON(map[string]any{"recorded": false, "error": "db error (table may not exist)"})
		return
	}

	outputJSON(map[string]any{
		"recorded": true,
		"event_id": eventID,
	})
}

func newHookID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", os.Getpid())
	}
	return hex.EncodeToString(b)
}

func truncateJSONMetadata(cwd, raw string) string {
	meta, err := json.Marshal(map[string]any{
		"cwd":        cwd,
		"raw_length": len(raw),
	})
	if err != nil {
		return "{}"
	}
	if len(meta) > 1024 {
		meta = meta[:1024]
	}
	return string(meta)
}
