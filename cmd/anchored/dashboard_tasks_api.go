package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/session"
)

// taskView is the dashboard-facing shape of a task thread. It enriches the
// stored thread with human-readable project names (resolved from the projects
// table) and cheap counts the board renders without a second round-trip.
type taskView struct {
	TaskKey      string   `json:"task_key"`
	ExternalRef  string   `json:"external_ref"`
	Status       string   `json:"status"`
	ProjectIDs   []string `json:"project_ids"`
	ProjectNames []string `json:"project_names"`
	Journal      []string `json:"journal"`
	SessionIDs   []string `json:"session_ids"`
	JournalCount int      `json:"journal_count"`
	SessionCount int      `json:"session_count"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	// LiveSessionID is the id of a currently-live session linked to this task,
	// if any — lets the board show "active now" and jump to the session.
	LiveSessionID string `json:"live_session_id,omitempty"`
}

func (a *dashboardAPI) toTaskView(t session.TaskThread, names map[string]string) taskView {
	pn := make([]string, 0, len(t.ProjectIDs))
	for _, id := range t.ProjectIDs {
		if n := names[id]; n != "" {
			pn = append(pn, n)
		} else {
			pn = append(pn, id)
		}
	}
	return taskView{
		TaskKey:      t.TaskKey,
		ExternalRef:  t.ExternalRef,
		Status:       t.Status,
		ProjectIDs:   t.ProjectIDs,
		ProjectNames: pn,
		Journal:      t.Journal,
		SessionIDs:   t.SessionIDs,
		JournalCount: len(t.Journal),
		SessionCount: len(t.SessionIDs),
		CreatedAt:    t.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    t.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// projectNameMap builds an id→name lookup so the board can label project chips.
// Best-effort: on query failure it returns an empty map and views fall back to
// showing raw project IDs.
func (a *dashboardAPI) projectNameMap(r *http.Request) map[string]string {
	names := map[string]string{}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id, name FROM projects`)
	if err != nil {
		return names
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			names[id] = name
		}
	}
	return names
}

// handleTasksList returns every task thread (optionally filtered by ?status=),
// most-recently-touched first, grouped-ready for the Kanban board.
func (a *dashboardAPI) handleTasksList(w http.ResponseWriter, r *http.Request) {
	threads, err := a.sessions.ListTaskThreads(r.Context(), r.URL.Query().Get("status"), queryInt(r, "limit", 500, 2000))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "tasks: %v", err)
		return
	}
	names := a.projectNameMap(r)
	liveByTask := a.liveSessionByTask(r.Context()) // task_key -> live session id
	out := make([]taskView, 0, len(threads))
	for _, t := range threads {
		v := a.toTaskView(t, names)
		v.LiveSessionID = liveByTask[t.TaskKey]
		out = append(out, v)
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

// liveSessionByTask maps each linked task to a currently-live session id.
// Best-effort: returns an empty map on error so the board still renders.
func (a *dashboardAPI) liveSessionByTask(ctx context.Context) map[string]string {
	m := map[string]string{}
	live, err := a.sessions.GetLiveSessions(ctx, cockpitIdleThreshold)
	if err != nil {
		return m
	}
	for _, s := range live {
		if s.TaskKey != "" {
			m[s.TaskKey] = s.ID
		}
	}
	return m
}

func (a *dashboardAPI) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	key := strings.ToUpper(strings.TrimSpace(r.PathValue("key")))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing key")
		return
	}
	t, err := a.sessions.GetTaskThread(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task: %v", err)
		return
	}
	if t == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	dashWriteJSON(w, http.StatusOK, a.toTaskView(*t, a.projectNameMap(r)))
}

type taskCreateReq struct {
	TaskKey     string   `json:"task_key"`
	ExternalRef string   `json:"external_ref"`
	ProjectIDs  []string `json:"project_ids"`
	JournalNote string   `json:"journal_note"`
}

// handleTaskCreate creates (or merges into) a task thread. Mirrors the upsert
// semantics of `anchored task start`: a brand-new key is created active, and
// posting an existing key merges the supplied fields.
func (a *dashboardAPI) handleTaskCreate(w http.ResponseWriter, r *http.Request) {
	var req taskCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	key := strings.ToUpper(strings.TrimSpace(req.TaskKey))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "task_key is required")
		return
	}

	// UpsertTaskThread merges one project per call; seed the first here and fold
	// in any extras so a multi-project create lands them all.
	delta := session.TaskThreadDelta{ExternalRef: req.ExternalRef, JournalNote: req.JournalNote}
	if len(req.ProjectIDs) > 0 {
		delta.ProjectID = strings.TrimSpace(req.ProjectIDs[0])
	}
	t, err := a.sessions.UpsertTaskThread(r.Context(), key, delta)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create task: %v", err)
		return
	}
	if len(req.ProjectIDs) > 1 {
		for _, pid := range req.ProjectIDs[1:] {
			if pid = strings.TrimSpace(pid); pid == "" {
				continue
			}
			if t, err = a.sessions.UpsertTaskThread(r.Context(), key, session.TaskThreadDelta{ProjectID: pid}); err != nil {
				writeErr(w, http.StatusInternalServerError, "attach project: %v", err)
				return
			}
		}
	}
	dashWriteJSON(w, http.StatusCreated, a.toTaskView(*t, a.projectNameMap(r)))
}

type taskUpdateReq struct {
	Status      *string `json:"status"`
	ExternalRef *string `json:"external_ref"`
	JournalNote *string `json:"journal_note"`
	ProjectID   *string `json:"project_id"`
}

// handleTaskUpdate applies a partial update: a status transition (drag between
// board columns) and/or a journal note / external ref / project attach. Status
// changes go through SetTaskStatus so terminal states can be reopened from the
// board, which the automation-only Upsert path deliberately refuses.
func (a *dashboardAPI) handleTaskUpdate(w http.ResponseWriter, r *http.Request) {
	key := strings.ToUpper(strings.TrimSpace(r.PathValue("key")))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing key")
		return
	}
	var req taskUpdateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: %v", err)
		return
	}

	existing, err := a.sessions.GetTaskThread(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task: %v", err)
		return
	}
	if existing == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if req.Status != nil {
		switch *req.Status {
		case session.TaskStatusActive, session.TaskStatusPaused, session.TaskStatusDone, session.TaskStatusCancelled:
		default:
			writeErr(w, http.StatusBadRequest, "invalid status %q", *req.Status)
			return
		}
	}

	// UpdateTaskThread applies the free-form fields and the status transition in
	// the order that preserves intent: a closing note is recorded before a
	// done/cancel, and a reopen happens before merging into a terminal thread.
	delta := session.TaskThreadDelta{}
	if req.ExternalRef != nil {
		delta.ExternalRef = *req.ExternalRef
	}
	if req.JournalNote != nil {
		delta.JournalNote = *req.JournalNote
	}
	if req.ProjectID != nil {
		delta.ProjectID = strings.TrimSpace(*req.ProjectID)
	}
	newStatus := ""
	if req.Status != nil {
		newStatus = *req.Status
	}
	t, err := a.sessions.UpdateTaskThread(r.Context(), key, delta, newStatus)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "update task: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, a.toTaskView(*t, a.projectNameMap(r)))
}

// handleTaskDelete is a soft cancel (status → cancelled), matching the
// read-mostly posture of the dashboard: threads are never hard-deleted here.
func (a *dashboardAPI) handleTaskDelete(w http.ResponseWriter, r *http.Request) {
	key := strings.ToUpper(strings.TrimSpace(r.PathValue("key")))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing key")
		return
	}
	// Distinguish "no such task" (404) from a real backend failure (500): probe
	// existence first, then cancel.
	existing, err := a.sessions.GetTaskThread(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task: %v", err)
		return
	}
	if existing == nil {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if err := a.sessions.SetTaskStatus(r.Context(), key, session.TaskStatusCancelled); err != nil {
		writeErr(w, http.StatusInternalServerError, "cancel task: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
