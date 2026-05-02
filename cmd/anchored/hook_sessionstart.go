package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
)

func runHookSessionStart(args []string) {
	fs := newFlagSet("hook sessionstart")
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
		SessionID string `json:"session_id"`
		Directory string `json:"directory"`
	}
	_ = json.Unmarshal(content, &input)

	sid := *sessionID
	if sid == "" {
		sid = input.SessionID
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
	projectID := svc.ResolveProject(cwdVal)

	ctx := context.Background()
	db := svc.StoreDB()

	resumeContext := ""
	row := db.QueryRowContext(ctx,
		`SELECT summary FROM session_events WHERE event_type = 'precompact_snapshot' AND (project_id = ? OR project_id = '') ORDER BY created_at DESC LIMIT 1`,
		projectID,
	)
	_ = row.Scan(&resumeContext)

	type recentEvent struct {
		EventType string `json:"event_type"`
		ToolName  string `json:"tool_name"`
		Summary   string `json:"summary"`
		CreatedAt string `json:"created_at"`
	}

	var recent []recentEvent
	rows, err := db.QueryContext(ctx,
		`SELECT event_type, tool_name, summary, created_at FROM session_events
		 WHERE priority <= 2 AND (project_id = ? OR project_id = '') ORDER BY created_at DESC LIMIT 20`,
		projectID,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e recentEvent
			if err := rows.Scan(&e.EventType, &e.ToolName, &e.Summary, &e.CreatedAt); err == nil {
				recent = append(recent, e)
			}
		}
		_ = rows.Err()
	}

	suggestion := "No previous session context found"
	if resumeContext != "" {
		suggestion = "Resume from previous context"
	}

	outputJSON(map[string]any{
		"resume_context": resumeContext,
		"recent_events":  recent,
		"suggestions":    suggestion,
	})
}
