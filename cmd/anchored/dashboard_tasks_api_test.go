package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jholhewres/anchored/pkg/session"
)

// doJSONReq issues a request with an optional JSON body and returns the status
// code, decoding the response into out when non-nil (any 2xx).
func doJSONReq(t *testing.T, srv *httptest.Server, method, path string, body, out any) int {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode
}

func newTaskDashboardAPI(t *testing.T) *dashboardAPI {
	t.Helper()
	svc := newDashboardTestSvc(t)
	return &dashboardAPI{
		svc:      svc,
		db:       svc.StoreDB(),
		sessions: session.NewManager(svc.StoreDB(), slog.Default()),
		logger:   slog.Default(),
	}
}

// TestDashboardTasksAPI_CRUD walks the full task-thread lifecycle through the
// HTTP surface: create → list → get → status transition → journal note →
// soft-cancel, asserting the board-facing contract at each step.
func TestDashboardTasksAPI_CRUD(t *testing.T) {
	api := newTaskDashboardAPI(t)
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	// create (lowercase key must be normalized to uppercase)
	var created map[string]any
	if c := doJSONReq(t, srv, "POST", "/api/tasks",
		map[string]any{"task_key": "proj-42", "external_ref": "JIRA-42", "journal_note": "kickoff"},
		&created); c != http.StatusCreated {
		t.Fatalf("create: %d", c)
	}
	if created["task_key"] != "PROJ-42" {
		t.Errorf("task_key = %v, want PROJ-42", created["task_key"])
	}
	if created["status"] != session.TaskStatusActive {
		t.Errorf("status = %v, want active", created["status"])
	}

	// list — the new thread shows up
	var list map[string]any
	if c := doJSONReq(t, srv, "GET", "/api/tasks", nil, &list); c != http.StatusOK {
		t.Fatalf("list: %d", c)
	}
	if num(list["count"]) != 1 {
		t.Fatalf("count = %v, want 1", list["count"])
	}

	// status filter that excludes the active thread returns empty
	var doneList map[string]any
	if c := doJSONReq(t, srv, "GET", "/api/tasks?status=done", nil, &doneList); c != http.StatusOK {
		t.Fatalf("list done: %d", c)
	}
	if num(doneList["count"]) != 0 {
		t.Errorf("done count = %v, want 0", doneList["count"])
	}

	// get single (case-insensitive key)
	var got map[string]any
	if c := doJSONReq(t, srv, "GET", "/api/tasks/proj-42", nil, &got); c != http.StatusOK {
		t.Fatalf("get: %d", c)
	}
	if jc := num(got["journal_count"]); jc != 1 {
		t.Errorf("journal_count = %d, want 1 (kickoff)", jc)
	}

	// PATCH: move to done + append a journal note
	var patched map[string]any
	if c := doJSONReq(t, srv, "PATCH", "/api/tasks/PROJ-42",
		map[string]any{"status": "done", "journal_note": "shipped"}, &patched); c != http.StatusOK {
		t.Fatalf("patch: %d", c)
	}
	if patched["status"] != session.TaskStatusDone {
		t.Errorf("status after patch = %v, want done", patched["status"])
	}
	if jc := num(patched["journal_count"]); jc != 2 {
		t.Errorf("journal_count after note = %d, want 2", jc)
	}

	// DELETE = soft cancel
	if c := doJSONReq(t, srv, "DELETE", "/api/tasks/PROJ-42", nil, nil); c != http.StatusNoContent {
		t.Fatalf("delete: %d", c)
	}
	th, err := api.sessions.GetTaskThread(context.Background(), "PROJ-42")
	if err != nil || th == nil {
		t.Fatalf("reload after cancel: th=%v err=%v", th, err)
	}
	if th.Status != session.TaskStatusCancelled {
		t.Errorf("status after delete = %q, want cancelled", th.Status)
	}
}

// TestDashboardTasksAPI_Errors covers the validation and not-found edges.
func TestDashboardTasksAPI_Errors(t *testing.T) {
	api := newTaskDashboardAPI(t)
	srv := httptest.NewServer(api.routes())
	defer srv.Close()

	// create without key -> 400
	if c := doJSONReq(t, srv, "POST", "/api/tasks", map[string]any{"external_ref": "x"}, nil); c != http.StatusBadRequest {
		t.Errorf("create no key: %d, want 400", c)
	}
	// get missing -> 404
	if c := doJSONReq(t, srv, "GET", "/api/tasks/NOPE-1", nil, nil); c != http.StatusNotFound {
		t.Errorf("get missing: %d, want 404", c)
	}
	// patch missing -> 404
	if c := doJSONReq(t, srv, "PATCH", "/api/tasks/NOPE-1", map[string]any{"status": "done"}, nil); c != http.StatusNotFound {
		t.Errorf("patch missing: %d, want 404", c)
	}
	// invalid status on an existing thread -> 400
	if c := doJSONReq(t, srv, "POST", "/api/tasks", map[string]any{"task_key": "OK-1"}, nil); c != http.StatusCreated {
		t.Fatalf("seed create: %d", c)
	}
	if c := doJSONReq(t, srv, "PATCH", "/api/tasks/OK-1", map[string]any{"status": "bogus"}, nil); c != http.StatusBadRequest {
		t.Errorf("patch bad status: %d, want 400", c)
	}
}
