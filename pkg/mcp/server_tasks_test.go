package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
)

func newTaskTestServer(t *testing.T) (*Server, *session.Manager) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "test.db")
	cfg.Embedding.Provider = "none"

	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Close)

	mgr := session.NewManager(svc.StoreDB(), slog.Default())
	srv := NewServer(svc, nil, mgr, nil, cfg, "test", slog.Default())
	return srv, mgr
}

func callTask(t *testing.T, s *Server, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return s.callTool(context.Background(), "anchored_task", raw)
}

// TestToolTask_LifecycleAndLinking exercises start→note→status through the MCP
// dispatch, asserting the session is linked and the design's reactivation rule
// holds (start reactivates a paused thread; note does not).
func TestToolTask_LifecycleAndLinking(t *testing.T) {
	srv, mgr := newTaskTestServer(t)
	ctx := context.Background()

	// start links the current session
	if _, err := callTask(t, srv, map[string]any{
		"action": "start", "key": "proj-5", "session_id": "sess-A", "external_ref": "GH-5",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	th, _ := mgr.GetTaskThread(ctx, "PROJ-5")
	if th == nil || th.Status != session.TaskStatusActive {
		t.Fatalf("after start: %+v", th)
	}
	if !contains(th.SessionIDs, "sess-A") {
		t.Errorf("session not linked on start: %v", th.SessionIDs)
	}

	// pause it
	if _, err := callTask(t, srv, map[string]any{"action": "status", "key": "PROJ-5", "status": "paused"}); err != nil {
		t.Fatalf("pause: %v", err)
	}

	// note links a new session but must NOT reactivate the paused thread
	if _, err := callTask(t, srv, map[string]any{
		"action": "note", "key": "PROJ-5", "note": "still blocked", "session_id": "sess-B",
	}); err != nil {
		t.Fatalf("note: %v", err)
	}
	th, _ = mgr.GetTaskThread(ctx, "PROJ-5")
	if th.Status != session.TaskStatusPaused {
		t.Errorf("note reactivated a paused task: status=%q, want paused", th.Status)
	}
	if !contains(th.SessionIDs, "sess-B") {
		t.Errorf("note did not link session: %v", th.SessionIDs)
	}
	if len(th.Journal) == 0 || th.Journal[0] != "still blocked" {
		t.Errorf("journal not recorded: %v", th.Journal)
	}

	// start again reactivates
	if _, err := callTask(t, srv, map[string]any{"action": "start", "key": "PROJ-5", "session_id": "sess-C"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	th, _ = mgr.GetTaskThread(ctx, "PROJ-5")
	if th.Status != session.TaskStatusActive {
		t.Errorf("start did not reactivate: status=%q", th.Status)
	}

	// done
	if _, err := callTask(t, srv, map[string]any{"action": "status", "key": "PROJ-5", "status": "done"}); err != nil {
		t.Fatalf("done: %v", err)
	}
	th, _ = mgr.GetTaskThread(ctx, "PROJ-5")
	if th.Status != session.TaskStatusDone {
		t.Errorf("status=%q, want done", th.Status)
	}
}

func TestToolTask_ListGetAndErrors(t *testing.T) {
	srv, _ := newTaskTestServer(t)

	callTask(t, srv, map[string]any{"action": "start", "key": "A-1"})
	callTask(t, srv, map[string]any{"action": "start", "key": "B-2"})

	out, err := callTask(t, srv, map[string]any{"action": "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "A-1") || !strings.Contains(out, "B-2") {
		t.Errorf("list missing threads: %q", out)
	}

	out, err = callTask(t, srv, map[string]any{"action": "get", "key": "A-1"})
	if err != nil || !strings.Contains(out, "A-1") {
		t.Errorf("get: out=%q err=%v", out, err)
	}

	// get missing → friendly message, not error
	if out, err = callTask(t, srv, map[string]any{"action": "get", "key": "NOPE-9"}); err != nil {
		t.Errorf("get missing should not error: %v", err)
	}

	// start without key → error
	if _, err = callTask(t, srv, map[string]any{"action": "start"}); err == nil {
		t.Error("start without key should error")
	}
	// unknown action → error
	if _, err = callTask(t, srv, map[string]any{"action": "frobnicate", "key": "A-1"}); err == nil {
		t.Error("unknown action should error")
	}
}

// TestToolTask_NoPhantomOnStatus guards M1: setting a terminal status on a
// non-existent key must not create a phantom closed task.
func TestToolTask_NoPhantomOnStatus(t *testing.T) {
	srv, mgr := newTaskTestServer(t)
	out, err := callTask(t, srv, map[string]any{"action": "status", "key": "GHOST-1", "status": "done"})
	if err != nil {
		t.Fatalf("status on missing: %v", err)
	}
	if th, _ := mgr.GetTaskThread(context.Background(), "GHOST-1"); th != nil {
		t.Errorf("phantom task created: %+v", th)
	}
	if !strings.Contains(out, "No task thread") {
		t.Errorf("unexpected message: %q", out)
	}
}

// TestToolTask_TerminalFreezeIsHonest guards M2: start/note on a done task must
// not falsely report success (the delta is frozen out).
func TestToolTask_TerminalFreezeIsHonest(t *testing.T) {
	srv, _ := newTaskTestServer(t)
	callTask(t, srv, map[string]any{"action": "start", "key": "FRZ-1"})
	callTask(t, srv, map[string]any{"action": "status", "key": "FRZ-1", "status": "done"})

	out, _ := callTask(t, srv, map[string]any{"action": "note", "key": "FRZ-1", "note": "late note"})
	if !strings.Contains(out, "not recorded") {
		t.Errorf("note on done task should report it was not recorded, got: %q", out)
	}
	out, _ = callTask(t, srv, map[string]any{"action": "start", "key": "FRZ-1"})
	if !strings.Contains(out, "nothing changed") {
		t.Errorf("start on done task should report no-op, got: %q", out)
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
