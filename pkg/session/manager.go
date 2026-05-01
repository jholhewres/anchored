package session

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

)

type Session struct {
	ID              string     `json:"id"`
	ProjectID       *string    `json:"project_id,omitempty"`
	Source          string     `json:"source"`
	SourceSessionID string     `json:"source_session_id,omitempty"`
	Title           string     `json:"title,omitempty"`
	Directory       string     `json:"directory,omitempty"`
	CreatedAt       *time.Time `json:"created_at,omitempty"`
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	MessageCount    int        `json:"message_count"`
	SourceTool      string     `json:"source_tool,omitempty"`
	Metadata        any        `json:"metadata,omitempty"`
}

type Manager struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewManager(db *sql.DB, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{db: db, logger: logger}
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// StartSession creates a new live session or resumes an existing active one
// matching sourceSessionID.
func (m *Manager) StartSession(ctx context.Context, sourceTool, sourceSessionID, projectID, directory string) (string, error) {
	// Resume existing active session if sourceSessionID is provided.
	if sourceSessionID != "" {
		var existingID string
		err := m.db.QueryRowContext(ctx,
			`SELECT id FROM sessions WHERE source_session_id = ? AND ended_at IS NULL`,
			sourceSessionID,
		).Scan(&existingID)
		if err == nil {
			// Found active session — bump activity.
			_, err = m.db.ExecContext(ctx,
				`UPDATE sessions SET last_activity_at = CURRENT_TIMESTAMP WHERE id = ?`,
				existingID,
			)
			if err != nil {
				return "", fmt.Errorf("update activity on resume: %w", err)
			}
			return existingID, nil
		}
		if err != sql.ErrNoRows {
			return "", fmt.Errorf("lookup active session: %w", err)
		}
		// errNoRows → fall through to create new.
	}

	id := newUUID()

	var pid *string
	if projectID != "" {
		pid = &projectID
	}

	_, err := m.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, source, source_session_id, title, directory, created_at, message_count, last_activity_at, source_tool)
		 VALUES (?, ?, 'live', ?, '', ?, CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, ?)`,
		id, pid, sourceSessionID, directory, sourceTool,
	)
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}

	return id, nil
}

// RecordActivity bumps last_activity_at and increments message_count.
func (m *Manager) RecordActivity(ctx context.Context, sessionID string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET last_activity_at = CURRENT_TIMESTAMP, message_count = message_count + 1 WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("record activity: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	return nil
}

// EndSession sets ended_at on the session.
func (m *Manager) EndSession(ctx context.Context, sessionID string) error {
	res, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET ended_at = CURRENT_TIMESTAMP WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("end session: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	return nil
}

// GetActiveSession returns the active (not ended) session for sourceSessionID, or nil.
func (m *Manager) GetActiveSession(ctx context.Context, sourceSessionID string) (*Session, error) {
	row := m.db.QueryRowContext(ctx,
		`SELECT id, project_id, source, source_session_id, title, directory,
		        created_at, last_activity_at, ended_at, message_count, source_tool
		 FROM sessions WHERE source_session_id = ? AND ended_at IS NULL`,
		sourceSessionID,
	)

	var s Session
	var createdAt, lastActivityAt, endedAt sql.NullTime
	var projectID, title, directory, sourceTool sql.NullString

	err := row.Scan(
		&s.ID, &projectID, &s.Source, &s.SourceSessionID,
		&title, &directory,
		&createdAt, &lastActivityAt, &endedAt,
		&s.MessageCount, &sourceTool,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active session: %w", err)
	}

	if projectID.Valid {
		s.ProjectID = &projectID.String
	}
	if title.Valid {
		s.Title = title.String
	}
	if directory.Valid {
		s.Directory = directory.String
	}
	if createdAt.Valid {
		t := createdAt.Time
		s.CreatedAt = &t
	}
	if lastActivityAt.Valid {
		t := lastActivityAt.Time
		s.LastActivityAt = &t
	}
	if endedAt.Valid {
		t := endedAt.Time
		s.EndedAt = &t
	}
	if sourceTool.Valid {
		s.SourceTool = sourceTool.String
	}
	return &s, nil
}

// EndStaleSessions ends sessions with no activity for longer than maxAge.
// Returns the count of sessions closed.
func (m *Manager) EndStaleSessions(ctx context.Context, maxAge time.Duration) (int, error) {
	modifier := fmt.Sprintf("-%d seconds", int(maxAge.Seconds()))
	res, err := m.db.ExecContext(ctx,
		`UPDATE sessions SET ended_at = CURRENT_TIMESTAMP
		 WHERE ended_at IS NULL AND last_activity_at < datetime('now', ?)`,
		modifier,
	)
	if err != nil {
		return 0, fmt.Errorf("end stale sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SessionStats returns total and active session counts.
func (m *Manager) SessionStats(ctx context.Context) (total int, active int, err error) {
	err = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&total)
	if err != nil {
		return 0, 0, fmt.Errorf("count total sessions: %w", err)
	}
	err = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE ended_at IS NULL`).Scan(&active)
	if err != nil {
		return 0, 0, fmt.Errorf("count active sessions: %w", err)
	}
	return total, active, nil
}

