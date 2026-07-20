package session

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Task thread statuses. Lifecycle is driven by explicit commands or
// automation (branch inference, done-on-merge later) — never by interactive
// prompts.
const (
	TaskStatusBacklog   = "backlog"
	TaskStatusActive    = "active"
	TaskStatusPaused    = "paused"
	TaskStatusDone      = "done"
	TaskStatusCancelled = "cancelled"
)

// TaskThread is a first-class, cross-project unit of work: one Jira ticket /
// Trello card / branch-named task that may touch several repositories. It
// references — never duplicates — per-project data: memories keep their own
// project_id; the thread just ties the strands together.
type TaskThread struct {
	TaskKey     string
	ExternalRef string
	ProjectIDs  []string
	Journal     []string
	SessionIDs  []string
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TaskThreadDelta is a partial update merged into a thread (created on first
// touch). Empty fields are ignored.
type TaskThreadDelta struct {
	ProjectID   string
	SessionID   string
	JournalNote string
	ExternalRef string
	// KeepStatus suppresses the automatic paused→active reactivation that
	// otherwise fires when a project/session is attached. It lets a caller
	// record a session link or journal note against a paused thread WITHOUT
	// reopening it (e.g. the MCP `note` action), while `start` leaves it false
	// so resuming work still reactivates. Default false preserves the original
	// behavior for the branch-inference hook and the CLI.
	KeepStatus bool
}

// taskKeyRe matches ticket-style keys (PROJ-123) inside branch names or free
// text, case-insensitively — branches are often lowercase (feature/proj-123).
// The prefix requires >=2 LETTERS (no digits) so version-like fragments
// (v1-2, go1-21, i18n-42) don't spawn phantom threads on every sessionstart.
var taskKeyRe = regexp.MustCompile(`(?i)\b([a-z]{2,10}-[0-9]{1,6})\b`)

// InferTaskKey extracts a normalized (uppercase) ticket key from a branch
// name like "feature/PROJ-123-fix-login". Returns "" when none is present.
func InferTaskKey(branch string) string {
	m := taskKeyRe.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// GetTaskThread returns the thread for key, or nil when it does not exist.
func (m *Manager) GetTaskThread(ctx context.Context, key string) (*TaskThread, error) {
	row := m.db.QueryRowContext(ctx, `
		SELECT task_key, external_ref, project_ids, journal, session_ids, status, created_at, updated_at
		FROM task_threads WHERE task_key = ?`, key)
	return scanTaskThread(row)
}

// ActiveTaskThreads lists threads in active status, most recently touched
// first.
func (m *Manager) ActiveTaskThreads(ctx context.Context) ([]TaskThread, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT task_key, external_ref, project_ids, journal, session_ids, status, created_at, updated_at
		FROM task_threads WHERE status = ? ORDER BY updated_at DESC LIMIT 20`, TaskStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskThread
	for rows.Next() {
		t, err := scanTaskThreadRows(rows)
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// ListTaskThreads returns threads ordered most-recently-touched first. An empty
// status lists every thread regardless of state (for the dashboard board);
// a non-empty status filters to that lifecycle state. limit <= 0 applies a
// sane default cap.
func (m *Manager) ListTaskThreads(ctx context.Context, status string, limit int) ([]TaskThread, error) {
	if limit <= 0 {
		limit = 500
	}
	query := `
		SELECT task_key, external_ref, project_ids, journal, session_ids, status, created_at, updated_at
		FROM task_threads`
	args := []any{}
	if s := strings.TrimSpace(status); s != "" {
		query += ` WHERE status = ?`
		args = append(args, s)
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskThread{}
	for rows.Next() {
		t, err := scanTaskThreadRows(rows)
		if err != nil {
			continue
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// UpsertTaskThread merges delta into the thread, creating it (status active)
// on first touch. Resuming work on a paused thread reactivates it; done and
// cancelled threads are terminal for automation (only an explicit
// SetTaskStatus reopens them).
func (m *Manager) UpsertTaskThread(ctx context.Context, key string, delta TaskThreadDelta) (*TaskThread, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return nil, fmt.Errorf("task key is required")
	}

	// The read-merge-write below must be atomic: two concurrent upserts for
	// the same key would otherwise last-writer-win on the whole JSON blob and
	// silently drop one side's delta.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cur, err := scanTaskThread(tx.QueryRowContext(ctx, `
		SELECT task_key, external_ref, project_ids, journal, session_ids, status, created_at, updated_at
		FROM task_threads WHERE task_key = ?`, key))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	if cur == nil {
		cur = &TaskThread{TaskKey: key, Status: TaskStatusActive, CreatedAt: now}
	}
	// Terminal states are never silently reopened by automation.
	if cur.Status == TaskStatusDone || cur.Status == TaskStatusCancelled {
		return cur, nil
	}
	if !delta.KeepStatus && cur.Status == TaskStatusPaused && (delta.ProjectID != "" || delta.SessionID != "") {
		cur.Status = TaskStatusActive
	}

	if delta.ProjectID != "" {
		cur.ProjectIDs = mergeList(cur.ProjectIDs, []string{delta.ProjectID})
	}
	if delta.SessionID != "" {
		cur.SessionIDs = mergeList(cur.SessionIDs, []string{delta.SessionID})
	}
	if note := strings.TrimSpace(delta.JournalNote); note != "" {
		cur.Journal = prependJournal(cur.Journal, note)
	}
	if delta.ExternalRef != "" {
		cur.ExternalRef = delta.ExternalRef
	}
	cur.UpdatedAt = now

	_, err = tx.ExecContext(ctx, `
		INSERT INTO task_threads (task_key, external_ref, project_ids, journal, session_ids, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_key) DO UPDATE SET
			external_ref = excluded.external_ref,
			project_ids  = excluded.project_ids,
			journal      = excluded.journal,
			session_ids  = excluded.session_ids,
			status       = excluded.status,
			updated_at   = excluded.updated_at`,
		cur.TaskKey, cur.ExternalRef,
		encodeJSONList(cur.ProjectIDs), encodeJSONList(cur.Journal), encodeJSONList(cur.SessionIDs),
		cur.Status, cur.CreatedAt, cur.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return cur, tx.Commit()
}

// taskJournalMaxNotes caps the journal independently of the working-set
// lists — notes are prose, not a rolling focus window.
const taskJournalMaxNotes = 100

// prependJournal adds a note newest-first WITHOUT deduplication — journal
// semantics differ from mergeList (built for file paths, where
// case-insensitive dedupe makes sense; for prose it silently eats notes).
func prependJournal(journal []string, note string) []string {
	out := append([]string{note}, journal...)
	if len(out) > taskJournalMaxNotes {
		out = out[:taskJournalMaxNotes]
	}
	return out
}

// empty reports whether the delta carries no mergeable field (KeepStatus is a
// modifier, not content, so it does not count).
func (d TaskThreadDelta) empty() bool {
	return d.ProjectID == "" && d.SessionID == "" &&
		strings.TrimSpace(d.JournalNote) == "" && d.ExternalRef == ""
}

func isTerminalStatus(s string) bool {
	return s == TaskStatusDone || s == TaskStatusCancelled
}

// UpdateTaskThread applies an optional free-form delta and an optional status
// transition (newStatus == "" skips it) in the ORDER that preserves intent:
//
//   - Reopening a terminal thread (newStatus active/paused): the status change
//     runs FIRST, so the delta then lands on a live thread instead of being
//     silently dropped by the terminal-freeze guard in UpsertTaskThread.
//   - Otherwise (going terminal, or no status change): the delta runs FIRST, so
//     a closing note is recorded while the thread is still active before it is
//     moved to done/cancelled.
//
// The delta's status is always kept (KeepStatus) because the transition is
// owned by newStatus here — a merged session/project must never flip the status
// out from under an explicit request.
func (m *Manager) UpdateTaskThread(ctx context.Context, key string, delta TaskThreadDelta, newStatus string) (*TaskThread, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	delta.KeepStatus = true

	applyDelta := func() error {
		if delta.empty() {
			return nil
		}
		_, err := m.UpsertTaskThread(ctx, key, delta)
		return err
	}
	applyStatus := func() error {
		if newStatus == "" {
			return nil
		}
		return m.SetTaskStatus(ctx, key, newStatus)
	}

	var first, second func() error
	if newStatus != "" && !isTerminalStatus(newStatus) {
		first, second = applyStatus, applyDelta // reopen, then merge
	} else {
		first, second = applyDelta, applyStatus // merge (closing note), then close
	}
	if err := first(); err != nil {
		return nil, err
	}
	if err := second(); err != nil {
		return nil, err
	}
	return m.GetTaskThread(ctx, key)
}

// SetTaskStatus moves a thread to status (active|paused|done|cancelled).
func (m *Manager) SetTaskStatus(ctx context.Context, key, status string) error {
	switch status {
	case TaskStatusActive, TaskStatusPaused, TaskStatusDone, TaskStatusCancelled:
	default:
		return fmt.Errorf("invalid task status %q", status)
	}
	res, err := m.db.ExecContext(ctx,
		`UPDATE task_threads SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE task_key = ?`,
		status, strings.ToUpper(strings.TrimSpace(key)))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task %q not found", key)
	}
	return nil
}

func scanTaskThread(row *sql.Row) (*TaskThread, error) {
	var t TaskThread
	var projects, journal, sessions string
	err := row.Scan(&t.TaskKey, &t.ExternalRef, &projects, &journal, &sessions,
		&t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.ProjectIDs = decodeJSONList(projects)
	t.Journal = decodeJSONList(journal)
	t.SessionIDs = decodeJSONList(sessions)
	return &t, nil
}

func scanTaskThreadRows(rows *sql.Rows) (*TaskThread, error) {
	var t TaskThread
	var projects, journal, sessions string
	if err := rows.Scan(&t.TaskKey, &t.ExternalRef, &projects, &journal, &sessions,
		&t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.ProjectIDs = decodeJSONList(projects)
	t.Journal = decodeJSONList(journal)
	t.SessionIDs = decodeJSONList(sessions)
	return &t, nil
}
