package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
)

func newCockpitTestAPI(t *testing.T) (*dashboardAPI, *session.Manager) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := memory.Migrate(db); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(db, slog.Default())
	return &dashboardAPI{db: db, sessions: mgr, logger: slog.Default()}, mgr
}

func TestHandleSessionsLive(t *testing.T) {
	api, mgr := newCockpitTestAPI(t)
	ctx := context.Background()
	sid, _ := mgr.StartSession(ctx, "claude-code", "s1", "proj-1", "/work")
	_ = mgr.UpdateSessionMeta(ctx, sid, "glm", "glm-4.6")

	req := httptest.NewRequest(http.MethodGet, "/api/sessions/live", nil)
	rec := httptest.NewRecorder()
	api.handleSessionsLive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Sessions []session.LiveSession `json:"sessions"`
		Count    int                   `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 || got.Sessions[0].Provider != "glm" || got.Sessions[0].State != "active" {
		t.Fatalf("unexpected live sessions: %+v", got)
	}
}

func TestHandleSessionLinkUnlinkPromote(t *testing.T) {
	api, mgr := newCockpitTestAPI(t)
	ctx := context.Background()
	sid, _ := mgr.StartSession(ctx, "claude-code", "s1", "proj-1", "/work")
	if _, err := mgr.UpsertTaskThread(ctx, "API-1", session.TaskThreadDelta{ProjectID: "proj-1"}); err != nil {
		t.Fatal(err)
	}

	// Link
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/link", strings.NewReader(`{"task_key":"API-1"}`))
	req.SetPathValue("id", sid)
	rec := httptest.NewRecorder()
	api.handleSessionLink(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("link status=%d body=%s", rec.Code, rec.Body.String())
	}
	if key, _ := mgr.GetSessionTaskKey(ctx, sid); key != "API-1" {
		t.Fatalf("expected link to API-1, got %q", key)
	}

	// Link to missing task → 400
	bad := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/link", strings.NewReader(`{"task_key":"NOPE-9"}`))
	bad.SetPathValue("id", sid)
	brec := httptest.NewRecorder()
	api.handleSessionLink(brec, bad)
	if brec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing task, got %d", brec.Code)
	}

	// Unlink
	ureq := httptest.NewRequest(http.MethodDelete, "/api/sessions/"+sid+"/link", nil)
	ureq.SetPathValue("id", sid)
	urec := httptest.NewRecorder()
	api.handleSessionUnlink(urec, ureq)
	if urec.Code != http.StatusOK {
		t.Fatalf("unlink status=%d", urec.Code)
	}
	if key, _ := mgr.GetSessionTaskKey(ctx, sid); key != "" {
		t.Fatalf("expected unlinked, got %q", key)
	}

	// Promote
	preq := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/promote-task", strings.NewReader(`{"key":"NEW-7","project_id":"proj-1"}`))
	preq.SetPathValue("id", sid)
	prec := httptest.NewRecorder()
	api.handleSessionPromoteTask(prec, preq)
	if prec.Code != http.StatusCreated {
		t.Fatalf("promote status=%d body=%s", prec.Code, prec.Body.String())
	}
	if key, _ := mgr.GetSessionTaskKey(ctx, sid); key != "NEW-7" {
		t.Fatalf("expected linked to promoted NEW-7, got %q", key)
	}
}
