package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Session Cockpit: live-session state and the session<->task link that lets the
// dashboard show every open tool and bind an active session to a board task.
// StartSession stays as-is (backward compatible); provider/model/intent are set
// via lightweight setters right after it, so no caller signature changes.

// UpdateSessionMeta records the provider and model behind a session (e.g.
// "glm" / "glm-4.6" for Claude Code pointed at a GLM endpoint). Empty values
// are left untouched so partial detection never clobbers a known field.
func (m *Manager) UpdateSessionMeta(ctx context.Context, sessionID, provider, model string) error {
	if provider == "" && model == "" {
		return nil
	}
	_, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET
		   provider = CASE WHEN ?1 <> '' THEN ?1 ELSE provider END,
		   model    = CASE WHEN ?2 <> '' THEN ?2 ELSE model    END
		 WHERE id = ?3`,
		provider, model, sessionID,
	)
	if err != nil {
		return fmt.Errorf("update session meta: %w", err)
	}
	return nil
}

// SetSessionIntent stores the current detected intent for a session.
func (m *Manager) SetSessionIntent(ctx context.Context, sessionID, intent string) error {
	if intent == "" {
		return nil
	}
	_, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET intent = ? WHERE id = ?`, intent, sessionID)
	if err != nil {
		return fmt.Errorf("set session intent: %w", err)
	}
	return nil
}

// LiveEvent is a compact recent activity item shown on a session card.
type LiveEvent struct {
	EventType string    `json:"event_type"`
	ToolName  string    `json:"tool_name,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// LiveSession is a not-yet-ended session enriched for the cockpit view.
type LiveSession struct {
	ID             string      `json:"id"`
	Tool           string      `json:"tool"`     // source_tool (claude-code, cursor…)
	Provider       string      `json:"provider,omitempty"`
	Model          string      `json:"model,omitempty"`
	ProjectID      string      `json:"project_id,omitempty"`
	Directory      string      `json:"directory,omitempty"`
	Intent         string      `json:"intent,omitempty"`
	Title          string      `json:"title,omitempty"`
	State          string      `json:"state"` // "active" | "idle"
	LastActivityAt *time.Time  `json:"last_activity_at,omitempty"`
	MessageCount   int         `json:"message_count"`
	TaskKey        string      `json:"task_key,omitempty"`
	RecentEvents   []LiveEvent `json:"recent_events"`
}

// GetLiveSessions returns every not-ended session, newest activity first, with
// its linked task and a handful of recent events. State is "active" when the
// last activity is within idle; "idle" otherwise. Ending stale sessions is a
// separate concern (EndStaleSessions) run by the hub.
func (m *Manager) GetLiveSessions(ctx context.Context, idle time.Duration) ([]LiveSession, error) {
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.id, COALESCE(s.source_tool, s.source), COALESCE(s.provider,''),
		       COALESCE(s.model,''), COALESCE(s.project_id,''), COALESCE(s.directory,''),
		       COALESCE(s.intent,''), COALESCE(s.title,''), s.last_activity_at,
		       s.message_count, COALESCE(l.task_key,'')
		FROM sessions s
		LEFT JOIN session_task_link l ON l.session_id = s.id
		WHERE s.ended_at IS NULL
		ORDER BY s.last_activity_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query live sessions: %w", err)
	}
	defer rows.Close()

	cutoff := time.Now().Add(-idle)
	var out []LiveSession
	for rows.Next() {
		var ls LiveSession
		var lastActivity sql.NullTime
		if err := rows.Scan(&ls.ID, &ls.Tool, &ls.Provider, &ls.Model, &ls.ProjectID,
			&ls.Directory, &ls.Intent, &ls.Title, &lastActivity, &ls.MessageCount, &ls.TaskKey); err != nil {
			return nil, fmt.Errorf("scan live session: %w", err)
		}
		ls.State = "active"
		if lastActivity.Valid {
			t := lastActivity.Time
			ls.LastActivityAt = &t
			if t.Before(cutoff) {
				ls.State = "idle"
			}
		} else {
			ls.State = "idle"
		}
		ls.RecentEvents = []LiveEvent{}
		out = append(out, ls)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attach recent events per session (small N; live sessions are few).
	for i := range out {
		evs, err := m.recentSessionEvents(ctx, out[i].ID, 4)
		if err == nil {
			out[i].RecentEvents = evs
		}
	}
	return out, nil
}

func (m *Manager) recentSessionEvents(ctx context.Context, sessionID string, limit int) ([]LiveEvent, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT event_type, COALESCE(tool_name,''), COALESCE(summary,''), created_at
		FROM session_events WHERE session_id = ?
		ORDER BY created_at DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []LiveEvent
	for rows.Next() {
		var e LiveEvent
		var created sql.NullTime
		if err := rows.Scan(&e.EventType, &e.ToolName, &e.Summary, &created); err != nil {
			return nil, err
		}
		if created.Valid {
			e.CreatedAt = created.Time
		}
		evs = append(evs, e)
	}
	return evs, rows.Err()
}

// GetSessionTaskKey returns the task a session is linked to, or "" if none.
func (m *Manager) GetSessionTaskKey(ctx context.Context, sessionID string) (string, error) {
	var key string
	err := m.db.QueryRowContext(ctx,
		`SELECT task_key FROM session_task_link WHERE session_id = ?`, sessionID).Scan(&key)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return key, nil
}

// LinkTask binds a session to a task (1 session -> 1 task; replaces any prior
// link for that session). The task must already exist. It also records the
// session on the task thread so the thread knows which sessions worked on it.
func (m *Manager) LinkTask(ctx context.Context, sessionID, taskKey, linkedBy string) error {
	t, err := m.GetTaskThread(ctx, taskKey)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("task not found: %s", taskKey)
	}
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO session_task_link (session_id, task_key, linked_by, linked_at)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(session_id) DO UPDATE SET task_key = excluded.task_key,
		     linked_by = excluded.linked_by, linked_at = excluded.linked_at`,
		sessionID, t.TaskKey, linkedBy); err != nil {
		return fmt.Errorf("link task: %w", err)
	}
	// Attach the session to the thread without reopening a paused task.
	_, err = m.UpsertTaskThread(ctx, t.TaskKey, TaskThreadDelta{SessionID: sessionID, KeepStatus: true})
	return err
}

// UnlinkTask removes a session's task link.
func (m *Manager) UnlinkTask(ctx context.Context, sessionID string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM session_task_link WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("unlink task: %w", err)
	}
	return nil
}

// PromoteTaskFromSession creates a task from a session and links it. The caller
// supplies the key (e.g. inferred from the branch via InferTaskKey); an empty
// key gets a generated one. Returns the resulting task key.
func (m *Manager) PromoteTaskFromSession(ctx context.Context, sessionID, key, projectID, note string) (string, error) {
	if key == "" {
		key = "TASK-" + newUUID()[:8]
	}
	if _, err := m.UpsertTaskThread(ctx, key, TaskThreadDelta{
		ProjectID:   projectID,
		SessionID:   sessionID,
		JournalNote: note,
	}); err != nil {
		return "", err
	}
	if err := m.LinkTask(ctx, sessionID, key, "promote"); err != nil {
		return "", err
	}
	return key, nil
}

// AppendSessionJournal appends a note to the task linked to a session. No-op
// (no error) when the session isn't linked to any task.
func (m *Manager) AppendSessionJournal(ctx context.Context, sessionID, note string) error {
	key, err := m.GetSessionTaskKey(ctx, sessionID)
	if err != nil || key == "" {
		return err
	}
	_, err = m.UpsertTaskThread(ctx, key, TaskThreadDelta{JournalNote: note, KeepStatus: true})
	return err
}
