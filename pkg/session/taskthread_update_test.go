package session

import (
	"context"
	"database/sql"
	"log/slog"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func newTaskTestMgr(t *testing.T) *Manager {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE task_threads (
		task_key TEXT PRIMARY KEY, external_ref TEXT NOT NULL DEFAULT '',
		project_ids TEXT NOT NULL DEFAULT '[]', journal TEXT NOT NULL DEFAULT '[]',
		session_ids TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'active',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	return NewManager(db, slog.Default())
}

// TestUpdateTaskThread_ClosingNoteBeforeDone: marking a task done WITH a closing
// note in one call must record the note (it lands while the thread is still
// active, before the terminal transition).
func TestUpdateTaskThread_ClosingNoteBeforeDone(t *testing.T) {
	m := newTaskTestMgr(t)
	ctx := context.Background()
	if _, err := m.UpsertTaskThread(ctx, "T-1", TaskThreadDelta{JournalNote: "start"}); err != nil {
		t.Fatal(err)
	}
	t2, err := m.UpdateTaskThread(ctx, "T-1", TaskThreadDelta{JournalNote: "shipped"}, TaskStatusDone)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != TaskStatusDone {
		t.Errorf("status = %q, want done", t2.Status)
	}
	if len(t2.Journal) != 2 || t2.Journal[0] != "shipped" {
		t.Errorf("closing note dropped: journal=%v", t2.Journal)
	}
}

// TestUpdateTaskThread_ReopenThenNote: reopening a DONE task (status=active)
// with a note in one call must reopen FIRST so the note is not swallowed by the
// terminal-freeze guard.
func TestUpdateTaskThread_ReopenThenNote(t *testing.T) {
	m := newTaskTestMgr(t)
	ctx := context.Background()
	if _, err := m.UpsertTaskThread(ctx, "T-2", TaskThreadDelta{JournalNote: "start"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(ctx, "T-2", TaskStatusDone); err != nil {
		t.Fatal(err)
	}
	t2, err := m.UpdateTaskThread(ctx, "T-2", TaskThreadDelta{JournalNote: "reopened"}, TaskStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != TaskStatusActive {
		t.Errorf("status = %q, want active (reopened)", t2.Status)
	}
	if len(t2.Journal) != 2 || t2.Journal[0] != "reopened" {
		t.Errorf("reopen note dropped: journal=%v", t2.Journal)
	}
}

// TestUpdateTaskThread_LinkKeepsPausedStatus: attaching a session via the update
// path must not reactivate a paused thread (KeepStatus is forced) when no status
// change is requested.
func TestUpdateTaskThread_LinkKeepsPausedStatus(t *testing.T) {
	m := newTaskTestMgr(t)
	ctx := context.Background()
	if _, err := m.UpsertTaskThread(ctx, "T-3", TaskThreadDelta{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(ctx, "T-3", TaskStatusPaused); err != nil {
		t.Fatal(err)
	}
	t2, err := m.UpdateTaskThread(ctx, "T-3", TaskThreadDelta{SessionID: "s2", JournalNote: "note"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != TaskStatusPaused {
		t.Errorf("status = %q, want paused (link must not reactivate)", t2.Status)
	}
	if !containsStr(t2.SessionIDs, "s2") {
		t.Errorf("session not linked: %v", t2.SessionIDs)
	}
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
