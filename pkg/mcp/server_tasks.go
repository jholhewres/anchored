package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jholhewres/anchored/pkg/session"
)

// toolTask backs the anchored_task MCP tool: a thin, guardrailed front over the
// session.Manager task-thread methods. It multiplexes on `action` so the MCP
// surface stays at one tool. Session/project linking is applied on every
// mutating action (start/note/status) per the design; `note` links WITHOUT
// reopening a paused thread (KeepStatus), while `start` reactivates.
func (s *Server) toolTask(ctx context.Context, args json.RawMessage) (string, error) {
	if s.sessions == nil {
		return "Task threads not available.", nil
	}
	var p struct {
		Action      string `json:"action"`
		Key         string `json:"key"`
		Note        string `json:"note"`
		Status      string `json:"status"`
		ExternalRef string `json:"external_ref"`
		SessionID   string `json:"session_id"`
		CWD         string `json:"cwd"`
		Limit       int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	p.CWD = defaultCWD(p.CWD)
	projectID := s.mem.ResolveProject(p.CWD)
	key := strings.ToUpper(strings.TrimSpace(p.Key))

	switch strings.ToLower(strings.TrimSpace(p.Action)) {
	case "start":
		if key == "" {
			return "", fmt.Errorf("key is required for action=start")
		}
		t, err := s.sessions.UpsertTaskThread(ctx, key, session.TaskThreadDelta{
			ProjectID: projectID, SessionID: p.SessionID,
			ExternalRef: p.ExternalRef, JournalNote: p.Note,
		})
		if err != nil {
			return "", err
		}
		// UpsertTaskThread freezes terminal threads: a start on a done/cancelled
		// task is a no-op and the delta was dropped. Say so honestly instead of
		// reporting "started".
		if isTerminal(t.Status) {
			return renderTask(fmt.Sprintf("Task is %s — nothing changed; reopen with action=status status=active first", t.Status), t), nil
		}
		return renderTask("Started/updated task", t), nil

	case "note":
		if key == "" {
			return "", fmt.Errorf("key is required for action=note")
		}
		if strings.TrimSpace(p.Note) == "" {
			return "", fmt.Errorf("note text is required for action=note")
		}
		// KeepStatus: link the session + record the note without reactivating a
		// paused thread — annotating is not resuming.
		t, err := s.sessions.UpsertTaskThread(ctx, key, session.TaskThreadDelta{
			ProjectID: projectID, SessionID: p.SessionID,
			JournalNote: p.Note, KeepStatus: true,
		})
		if err != nil {
			return "", err
		}
		// A terminal thread is frozen — the note was dropped. Report it rather
		// than falsely confirming the note was recorded.
		if isTerminal(t.Status) {
			return renderTask(fmt.Sprintf("Task is %s — note not recorded; reopen with action=status status=active first", t.Status), t), nil
		}
		return renderTask("Journal note added", t), nil

	case "status":
		if key == "" {
			return "", fmt.Errorf("key is required for action=status")
		}
		// Guard existence first: without this, a terminal target status would
		// take UpdateTaskThread's close-path (delta before status), and the
		// delta's UpsertTaskThread would CREATE the thread on first touch — so
		// a status change on a non-existent key would spawn a phantom
		// already-closed task.
		if existing, err := s.sessions.GetTaskThread(ctx, key); err != nil {
			return "", err
		} else if existing == nil {
			return fmt.Sprintf("No task thread %q.", key), nil
		}
		// UpdateTaskThread links the session (KeepStatus) and applies the status
		// in the intent-preserving order: a note lands before done/cancel, and a
		// reopen precedes the merge so it is not dropped on a terminal thread.
		t, err := s.sessions.UpdateTaskThread(ctx, key, session.TaskThreadDelta{
			ProjectID: projectID, SessionID: p.SessionID, JournalNote: p.Note,
		}, strings.ToLower(strings.TrimSpace(p.Status)))
		if err != nil {
			return "", err
		}
		return renderTask("Status updated", t), nil

	case "get":
		if key == "" {
			return "", fmt.Errorf("key is required for action=get")
		}
		t, err := s.sessions.GetTaskThread(ctx, key)
		if err != nil {
			return "", err
		}
		if t == nil {
			return fmt.Sprintf("No task thread %q.", key), nil
		}
		return renderTask("Task", t), nil

	case "list":
		limit := p.Limit
		if limit <= 0 {
			limit = 50
		}
		threads, err := s.sessions.ListTaskThreads(ctx, strings.ToLower(strings.TrimSpace(p.Status)), limit)
		if err != nil {
			return "", err
		}
		return renderTaskList(threads), nil

	default:
		return "", fmt.Errorf("unknown action %q (want start|note|status|list|get)", p.Action)
	}
}

func renderTask(prefix string, t *session.TaskThread) string {
	if t == nil {
		return prefix + ": (not found)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: **%s** [%s]", prefix, t.TaskKey, t.Status)
	if t.ExternalRef != "" {
		fmt.Fprintf(&b, " · ref: %s", t.ExternalRef)
	}
	fmt.Fprintf(&b, "\nprojects: %d · sessions: %d · journal: %d",
		len(t.ProjectIDs), len(t.SessionIDs), len(t.Journal))
	if len(t.Journal) > 0 {
		fmt.Fprintf(&b, "\nlatest note: %s", t.Journal[0])
	}
	return b.String()
}

func renderTaskList(threads []session.TaskThread) string {
	if len(threads) == 0 {
		return "No task threads."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task thread(s):\n", len(threads))
	for _, t := range threads {
		fmt.Fprintf(&b, "- %s [%s]", t.TaskKey, t.Status)
		if t.ExternalRef != "" {
			fmt.Fprintf(&b, " · %s", t.ExternalRef)
		}
		if len(t.Journal) > 0 {
			fmt.Fprintf(&b, " — %s", firstLine(t.Journal[0], 80))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// Truncate on a rune boundary so multi-byte UTF-8 (common in PT-BR notes)
	// is never sliced mid-codepoint.
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

func isTerminal(status string) bool {
	return status == session.TaskStatusDone || status == session.TaskStatusCancelled
}
