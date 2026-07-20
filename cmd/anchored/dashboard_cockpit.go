package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// cockpitIdleThreshold is how long a live session may go without activity before
// the cockpit shows it as "idle" rather than "active".
const cockpitIdleThreshold = 5 * time.Minute

// handleSessionsLive returns every not-ended session with its derived state,
// linked task and recent events — the data behind the Cockpit tab.
func (a *dashboardAPI) handleSessionsLive(w http.ResponseWriter, r *http.Request) {
	live, err := a.sessions.GetLiveSessions(r.Context(), cockpitIdleThreshold)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sessions/live: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"sessions": live, "count": len(live)})
}

// handleSessionLink binds a session to an existing task (1 session -> 1 task).
func (a *dashboardAPI) handleSessionLink(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing session id")
		return
	}
	var body struct {
		TaskKey string `json:"task_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.TaskKey) == "" {
		writeErr(w, http.StatusBadRequest, "task_key is required")
		return
	}
	if err := a.sessions.LinkTask(r.Context(), id, strings.ToUpper(strings.TrimSpace(body.TaskKey)), "dashboard"); err != nil {
		writeErr(w, http.StatusBadRequest, "link: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"linked": true, "session_id": id, "task_key": strings.ToUpper(strings.TrimSpace(body.TaskKey))})
}

// handleSessionUnlink removes a session's task link.
func (a *dashboardAPI) handleSessionUnlink(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing session id")
		return
	}
	if err := a.sessions.UnlinkTask(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "unlink: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"unlinked": true, "session_id": id})
}

// handleSessionPromoteTask creates a task from a session and links it.
func (a *dashboardAPI) handleSessionPromoteTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing session id")
		return
	}
	var body struct {
		Key       string `json:"key"`
		ProjectID string `json:"project_id"`
		Note      string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // all fields optional
	key, err := a.sessions.PromoteTaskFromSession(r.Context(), id, strings.ToUpper(strings.TrimSpace(body.Key)), body.ProjectID, body.Note)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "promote: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusCreated, map[string]any{"task_key": key, "session_id": id, "linked": true})
}
