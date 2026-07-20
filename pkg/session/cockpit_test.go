package session

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGetLiveSessions_StateAndTask(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db, nil)
	ctx := context.Background()

	// Active session (fresh activity).
	active, _ := m.StartSession(ctx, "claude-code", "s-active", "proj-1", "/work/a")
	_ = m.UpdateSessionMeta(ctx, active, "glm", "glm-4.6")
	_ = m.SetSessionIntent(ctx, active, "debug")

	// Idle session: backdate its activity beyond the threshold.
	idle, _ := m.StartSession(ctx, "cursor", "s-idle", "proj-2", "/work/b")
	if _, err := db.Exec(`UPDATE sessions SET last_activity_at = datetime('now','-30 minutes') WHERE id = ?`, idle); err != nil {
		t.Fatal(err)
	}

	// Ended session must not appear.
	ended, _ := m.StartSession(ctx, "codex", "s-ended", "", "/work/c")
	_ = m.EndSession(ctx, ended)

	live, err := m.GetLiveSessions(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("GetLiveSessions: %v", err)
	}
	if len(live) != 2 {
		t.Fatalf("expected 2 live sessions, got %d", len(live))
	}
	byID := map[string]LiveSession{}
	for _, s := range live {
		byID[s.ID] = s
	}
	if byID[active].State != "active" {
		t.Errorf("active session state = %q, want active", byID[active].State)
	}
	if byID[active].Provider != "glm" || byID[active].Model != "glm-4.6" || byID[active].Intent != "debug" {
		t.Errorf("meta not persisted: %+v", byID[active])
	}
	if byID[idle].State != "idle" {
		t.Errorf("idle session state = %q, want idle", byID[idle].State)
	}
	if _, ok := byID[ended]; ok {
		t.Error("ended session should not be live")
	}
}

func TestLinkTask_OneSessionOneTask(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db, nil)
	ctx := context.Background()

	sid, _ := m.StartSession(ctx, "claude-code", "s1", "proj-1", "/work")
	// Two tasks exist.
	if _, err := m.UpsertTaskThread(ctx, "API-1", TaskThreadDelta{ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpsertTaskThread(ctx, "API-2", TaskThreadDelta{ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}

	// Link to a non-existent task fails.
	if err := m.LinkTask(ctx, sid, "NOPE-9", "manual"); err == nil {
		t.Fatal("expected error linking to missing task")
	}

	// Link to API-1, then relink to API-2 → only one link remains (1->1).
	if err := m.LinkTask(ctx, sid, "API-1", "manual"); err != nil {
		t.Fatalf("link API-1: %v", err)
	}
	if err := m.LinkTask(ctx, sid, "API-2", "manual"); err != nil {
		t.Fatalf("relink API-2: %v", err)
	}
	key, _ := m.GetSessionTaskKey(ctx, sid)
	if key != "API-2" {
		t.Fatalf("expected link replaced to API-2, got %q", key)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_task_link WHERE session_id = ?`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 link row, got %d", n)
	}

	// Unlink removes it.
	if err := m.UnlinkTask(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if key, _ := m.GetSessionTaskKey(ctx, sid); key != "" {
		t.Fatalf("expected no link after unlink, got %q", key)
	}
}

func TestPromoteAndJournal(t *testing.T) {
	db := setupTestDB(t)
	m := NewManager(db, nil)
	ctx := context.Background()

	sid, _ := m.StartSession(ctx, "claude-code", "s1", "proj-1", "/work")
	key, err := m.PromoteTaskFromSession(ctx, sid, "PROJ-42", "proj-1", "created from session")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if key != "PROJ-42" {
		t.Fatalf("key = %q, want PROJ-42", key)
	}
	if got, _ := m.GetSessionTaskKey(ctx, sid); got != "PROJ-42" {
		t.Fatalf("session not linked to promoted task, got %q", got)
	}

	// Journal append lands on the linked task.
	if err := m.AppendSessionJournal(ctx, sid, "ran tests: 1 fail"); err != nil {
		t.Fatalf("append journal: %v", err)
	}
	th, _ := m.GetTaskThread(ctx, "PROJ-42")
	found := false
	for _, j := range th.Journal {
		if strings.Contains(j, "ran tests: 1 fail") {
			found = true
		}
	}
	if !found {
		t.Fatalf("journal note not recorded: %v", th.Journal)
	}

	// Journal on an unlinked session is a no-op (no error).
	other, _ := m.StartSession(ctx, "cursor", "s2", "", "/x")
	if err := m.AppendSessionJournal(ctx, other, "noise"); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}
