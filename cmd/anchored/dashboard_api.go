package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// dashboardAPI wires the HTTP routes to the memory.Service / raw DB. It is
// read-mostly (search, list, stats, KG, health); the only write path is the
// soft-delete of a single memory, mirroring `anchored forget`.
type dashboardAPI struct {
	svc    *memory.Service
	db     *sql.DB
	logger *slog.Logger
}

func (a *dashboardAPI) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/stats", a.handleStats)
	mux.HandleFunc("GET /api/timeline", a.handleTimeline)
	mux.HandleFunc("GET /api/keywords", a.handleKeywords)
	mux.HandleFunc("GET /api/entities", a.handleEntities)
	mux.HandleFunc("GET /api/memories", a.handleMemoriesList)
	mux.HandleFunc("GET /api/memories/{id}", a.handleMemoryGet)
	mux.HandleFunc("DELETE /api/memories/{id}", a.handleMemoryDelete)
	mux.HandleFunc("POST /api/memories/{id}/restore", a.handleMemoryRestore)
	mux.HandleFunc("GET /api/deleted", a.handleDeletedMemories)
	mux.HandleFunc("GET /api/search", a.handleSearch)
	mux.HandleFunc("GET /api/kg", a.handleKG)
	mux.HandleFunc("GET /api/sessions", a.handleSessions)
	mux.HandleFunc("GET /api/projects", a.handleProjects)
	mux.HandleFunc("GET /api/dream", a.handleDream)
	mux.HandleFunc("GET /api/artifacts", a.handleArtifacts)
	mux.HandleFunc("GET /api/chunks", a.handleChunks)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/imports", a.handleImports)
	mux.HandleFunc("GET /api/health", a.handleHealth)
	return a.recoverer(loggingMW(a.logger)(mux))
}

// --- helpers ---

func dashWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	dashWriteJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func queryInt(r *http.Request, key string, def, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

// loggingMW logs one line per request at debug level.
func loggingMW(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: 200}
			next.ServeHTTP(rw, r)
			logger.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rw.status, "dur_ms", time.Since(start).Milliseconds())
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// recoverer catches handler panics so a single bad row never kills the server.
func (a *dashboardAPI) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				a.logger.Error("dashboard panic", "path", r.URL.Path, "panic", rec)
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---

func (a *dashboardAPI) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := a.svc.Stats(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, stats)
}

func (a *dashboardAPI) handleTimeline(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if bucket != "month" {
		bucket = "day"
	}
	var expr string
	limit := 90
	if bucket == "month" {
		expr = "strftime('%Y-%m', created_at)"
		limit = 24
	} else {
		expr = "date(created_at)"
	}

	if r.URL.Query().Has("by_category") {
		// Stacked breakdown: one row per (period, category) so the frontend can
		// pivot into per-category series.
		crows, err := a.db.QueryContext(r.Context(), fmt.Sprintf(
			`SELECT %s AS period, category, COUNT(*) AS c
			 FROM memories
			 WHERE deleted_at IS NULL AND created_at IS NOT NULL
			 GROUP BY period, category
			 ORDER BY period DESC
			 LIMIT ?`, expr), 500)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "timeline: %v", err)
			return
		}
		defer crows.Close()
		type catpt struct {
			Period   string `json:"period"`
			Category string `json:"category"`
			Count    int    `json:"count"`
		}
		out := []catpt{}
		for crows.Next() {
			var period sql.NullString
			var category string
			var count int
			if err := crows.Scan(&period, &category, &count); err != nil {
				writeErr(w, http.StatusInternalServerError, "scan: %v", err)
				return
			}
			if !period.Valid || period.String == "" {
				continue
			}
			out = append(out, catpt{Period: period.String, Category: category, Count: count})
		}
		if err := crows.Err(); err != nil {
			writeErr(w, http.StatusInternalServerError, "timeline rows: %v", err)
			return
		}
		dashWriteJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "by_category": true, "points": out})
		return
	}

	rows, err := a.db.QueryContext(r.Context(), fmt.Sprintf(
		`SELECT %s AS period, COUNT(*) AS c
		 FROM memories
		 WHERE deleted_at IS NULL AND created_at IS NOT NULL
		 GROUP BY period
		 ORDER BY period DESC
		 LIMIT ?`, expr), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "timeline: %v", err)
		return
	}
	defer rows.Close()
	type point struct {
		Period string `json:"period"`
		Count  int    `json:"count"`
	}
	out := make([]point, 0, limit)
	for rows.Next() {
		var period sql.NullString
		var count int
		if err := rows.Scan(&period, &count); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		// date()/strftime() yield NULL for malformed timestamps in older
		// imports; skip those rather than failing the whole timeline.
		if !period.Valid || period.String == "" {
			continue
		}
		out = append(out, point{Period: period.String, Count: count})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "timeline rows: %v", err)
		return
	}
	// oldest first reads better as a left-to-right timeline
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "points": out})
}

// memoryRow is the trimmed shape sent to the list/table view (no full
// embedding blob, no oversized metadata).
type memoryRow struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	Content   string `json:"content"`
	Source    string `json:"source"`
	ProjectID string `json:"project_id"`
	CreatedAt string `json:"created_at"`
}

func (a *dashboardAPI) handleMemoriesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := queryInt(r, "limit", 50, 500)
	offset := queryInt(r, "offset", 0, 1<<30)

	// Build the WHERE clause dynamically so we get date-range + ordering that
	// memory.ListOptions doesn't expose, while keeping every value parameterized
	// (order column is whitelisted, never interpolated from input).
	var clauses []string
	var args []any
	add := func(clause string, v any) { clauses = append(clauses, clause); args = append(args, v) }
	if c := q.Get("category"); c != "" {
		add("category = ?", c)
	}
	if p := q.Get("project"); p != "" {
		add("project_id = ?", p)
	}
	if s := q.Get("source"); s != "" {
		add("source = ?", s)
	}
	if v := q.Get("since"); v != "" {
		// Compare on the YYYY-MM-DD prefix only. The corpus stores created_at in
		// several text shapes (RFC3339, "YYYY-MM-DD HH:MM:SS", Go's time.String,
		// …) — see dashDateFormats / toISO. A bare lexical comparison on the full
		// column is wrong across those shapes (a "until" of 2026-06-01 fails to
		// match "2026-06-01 10:00:00"). The date picker sends YYYY-MM-DD, which
		// is exactly the 10-char prefix every format shares, so substr makes the
		// range filter format-agnostic and correct.
		add("substr(created_at, 1, 10) >= ?", v)
	}
	if v := q.Get("until"); v != "" {
		add("substr(created_at, 1, 10) <= ?", v)
	}
	orderCol := "created_at"
	switch q.Get("order") {
	case "category", "source", "access_count", "updated_at":
		orderCol = q.Get("order")
	}
	dir := "DESC"
	if q.Get("dir") == "asc" {
		dir = "ASC"
	}
	// created_at is stored in several text shapes (RFC3339, "YYYY-MM-DD
	// HH:MM:SS", Go's time.String()…). Sort on datetime() so ISO and
	// SQLite-text rows compare chronologically instead of lexically; rows
	// datetime() can't parse (rare legacy imports) fall back to lexical order.
	orderExpr := orderCol
	if orderCol == "created_at" {
		orderExpr = "COALESCE(datetime(created_at), created_at)"
	}

	whereSQL := "deleted_at IS NULL"
	if len(clauses) > 0 {
		whereSQL += " AND " + strings.Join(clauses, " AND ")
	}

	var total int
	_ = a.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM memories WHERE "+whereSQL, args...).Scan(&total)

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := a.db.QueryContext(r.Context(),
		"SELECT id, category, content, source, COALESCE(project_id,''), created_at FROM memories WHERE "+whereSQL+
			" ORDER BY "+orderExpr+" "+dir+" LIMIT ? OFFSET ?", pageArgs...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list: %v", err)
		return
	}
	defer rows.Close()
	out := make([]memoryRow, 0, limit)
	for rows.Next() {
		var m memoryRow
		var created sql.NullString
		if err := rows.Scan(&m.ID, &m.Category, &m.Content, &m.Source, &m.ProjectID, &created); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		m.CreatedAt = toISO(created.String)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "list rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out), "total": total})
}

func (a *dashboardAPI) handleMemoryGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := a.svc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get: %v", err)
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	dashWriteJSON(w, http.StatusOK, m)
}

func (a *dashboardAPI) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	// Existence + not-already-deleted check via the cache-aware Get (which
	// filters deleted_at IS NULL), then mutate through Service.SoftForget so
	// the in-memory cache is invalidated and observers (sync/KG/events) fire —
	// a raw UPDATE here would silently skip them.
	m, err := a.svc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get: %v", err)
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "not found (already deleted?)")
		return
	}
	if err := a.svc.SoftForget(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *dashboardAPI) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		dashWriteJSON(w, http.StatusOK, map[string]any{"items": []any{}, "count": 0})
		return
	}
	results, err := a.svc.Search(r.Context(), q, memory.SearchOptions{
		MaxResults: queryInt(r, "limit", 50, 500),
		Category:   r.URL.Query().Get("category"),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search: %v", err)
		return
	}
	rows := make([]memoryRow, 0, len(results))
	for _, sr := range results {
		rows = append(rows, toMemoryRow(sr.Memory))
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": rows, "count": len(rows)})
}

// handleKeywords aggregates the per-memory keyword JSON arrays into a ranked
// list — the "what is this corpus about" signal for the overview.
func (a *dashboardAPI) handleKeywords(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 40, 200)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT keywords FROM memories WHERE keywords != '' AND keywords != '[]' AND deleted_at IS NULL LIMIT 5000`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "keywords: %v", err)
		return
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			continue
		}
		var list []string
		if err := json.Unmarshal([]byte(kw), &list); err != nil {
			continue
		}
		for _, k := range list {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !keywordStopWords[k] {
				counts[k]++
			}
		}
	}
	type pair struct {
		Word  string `json:"word"`
		Count int    `json:"count"`
	}
	out := make([]pair, 0, len(counts))
	for w, c := range counts {
		out = append(out, pair{Word: w, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Word < out[j].Word
	})
	if len(out) > limit {
		out = out[:limit]
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "keywords rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleEntities ranks KG entities by their degree (number of triples they
// participate in, as subject or object) — the most connected concepts.
func (a *dashboardAPI) handleEntities(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 40, 200)
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT e.name, SUM(c) AS deg FROM (
			SELECT subject_id AS eid, COUNT(*) AS c FROM kg_triples WHERE valid_to IS NULL GROUP BY subject_id
			UNION ALL
			SELECT object_id AS eid, COUNT(*) AS c FROM kg_triples WHERE valid_to IS NULL GROUP BY object_id
		) x JOIN kg_entities e ON e.id = x.eid
		GROUP BY e.id ORDER BY deg DESC LIMIT ?`, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "entities: %v", err)
		return
	}
	defer rows.Close()
	type ent struct {
		Name string `json:"name"`
		Deg  int    `json:"degree"`
	}
	out := []ent{}
	for rows.Next() {
		var e ent
		if err := rows.Scan(&e.Name, &e.Deg); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "entities rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleMemoryRestore undoes a soft-delete so the memory re-enters search/list
// results. The mutation goes through Service.Restore (cache invalidation +
// observer fan-out); only the existence-of-deleted-row check reads the DB.
func (a *dashboardAPI) handleMemoryRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing id")
		return
	}
	var one int
	_ = a.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM memories WHERE id = ? AND deleted_at IS NOT NULL`, id).Scan(&one)
	if one != 1 {
		writeErr(w, http.StatusNotFound, "not found (not deleted?)")
		return
	}
	if err := a.svc.Restore(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "restore: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeletedMemories lists soft-deleted memories (newest deletion first) so
// they can be reviewed and restored from the dashboard.
func (a *dashboardAPI) handleDeletedMemories(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 100, 500)
	offset := queryInt(r, "offset", 0, 1<<30)
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id, category, content, source, COALESCE(project_id,''), COALESCE(deleted_at,'')
		 FROM memories WHERE deleted_at IS NOT NULL
		 ORDER BY deleted_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "deleted: %v", err)
		return
	}
	defer rows.Close()
	type delRow struct {
		ID        string `json:"id"`
		Category  string `json:"category"`
		Content   string `json:"content"`
		Source    string `json:"source"`
		ProjectID string `json:"project_id"`
		DeletedAt string `json:"deleted_at"`
	}
	out := []delRow{}
	for rows.Next() {
		var d delRow
		if err := rows.Scan(&d.ID, &d.Category, &d.Content, &d.Source, &d.ProjectID, &d.DeletedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "deleted rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out, "count": len(out)})
}

type kgTriple struct {
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Confidence float64 `json:"confidence"`
	ProjectID  string  `json:"project_id,omitempty"`
}

// handleKG returns current (valid_to IS NULL) triples joined to entity/predicate
// names. The client builds nodes+edges for vis-network from this flat list.
func (a *dashboardAPI) handleKG(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := queryInt(r, "limit", 300, 2000)
	project := q.Get("project")
	var (
		rows *sql.Rows
		err  error
	)
	ctx := r.Context()
	if project != "" {
		rows, err = a.db.QueryContext(ctx, `
			SELECT sub.name, pred.name, obj.name, t.confidence, COALESCE(t.project_id,'')
			FROM kg_triples t
			JOIN kg_entities sub ON sub.id = t.subject_id
			JOIN kg_predicates pred ON pred.id = t.predicate_id
			JOIN kg_entities obj ON obj.id = t.object_id
			WHERE t.valid_to IS NULL AND t.project_id = ?
			ORDER BY t.confidence DESC, t.created_at DESC
			LIMIT ?`, project, limit)
	} else {
		rows, err = a.db.QueryContext(ctx, `
			SELECT sub.name, pred.name, obj.name, t.confidence, COALESCE(t.project_id,'')
			FROM kg_triples t
			JOIN kg_entities sub ON sub.id = t.subject_id
			JOIN kg_predicates pred ON pred.id = t.predicate_id
			JOIN kg_entities obj ON obj.id = t.object_id
			WHERE t.valid_to IS NULL
			ORDER BY t.confidence DESC, t.created_at DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "kg: %v", err)
		return
	}
	defer rows.Close()
	triples := make([]kgTriple, 0, limit)
	seen := map[string]int{}
	for rows.Next() {
		var t kgTriple
		if err := rows.Scan(&t.Subject, &t.Predicate, &t.Object, &t.Confidence, &t.ProjectID); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		triples = append(triples, t)
		seen[t.Subject]++
		seen[t.Object]++
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"triples": triples, "degrees": seen})
}

func (a *dashboardAPI) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var total, active int
	var lastActivity sql.NullTime
	_ = a.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN ended_at IS NULL THEN 1 ELSE 0 END),0),
		        MAX(last_activity_at)
		 FROM sessions`).Scan(&total, &active, &lastActivity)

	rows, err := a.db.QueryContext(ctx,
		`SELECT id, COALESCE(title,''), COALESCE(directory,''), COALESCE(source,''),
		        message_count, created_at, COALESCE(last_activity_at, created_at)
		 FROM sessions
		 ORDER BY COALESCE(last_activity_at, created_at) DESC
		 LIMIT 20`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "sessions: %v", err)
		return
	}
	defer rows.Close()
	type sess struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Directory    string `json:"directory"`
		Source       string `json:"source"`
		Messages     int    `json:"message_count"`
		CreatedAt    string `json:"created_at"`
		LastActivity string `json:"last_activity_at"`
	}
	recent := []sess{}
	for rows.Next() {
		var s sess
		var created, last sql.NullString
		if err := rows.Scan(&s.ID, &s.Title, &s.Directory, &s.Source, &s.Messages, &created, &last); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		s.CreatedAt = toISO(created.String)
		s.LastActivity = toISO(last.String)
		recent = append(recent, s)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "sessions rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{
		"total":         total,
		"active":        active,
		"last_activity": nullTimeStr(lastActivity),
		"recent":        recent,
	})
}

func (a *dashboardAPI) handleProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT p.id, p.name, p.path, COALESCE(p.remote_key,''),
		       COUNT(m.id) AS mems, p.created_at, MAX(m.created_at) AS last_activity
		FROM projects p
		LEFT JOIN memories m ON m.project_id = p.id AND m.deleted_at IS NULL
		GROUP BY p.id
		ORDER BY mems DESC
		LIMIT 200`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "projects: %v", err)
		return
	}
	defer rows.Close()
	type proj struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		RemoteKey    string `json:"remote_key"`
		Memories     int    `json:"memories"`
		CreatedAt    string `json:"created_at"`
		LastActivity string `json:"last_activity"`
	}
	out := []proj{}
	for rows.Next() {
		var p proj
		var created, last sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.RemoteKey, &p.Memories, &created, &last); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan: %v", err)
			return
		}
		p.CreatedAt = toISO(created.String)
		p.LastActivity = toISO(last.String)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "projects rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleDream reports consolidation status: latest run, aggregate action
// counts by type and status, and the most recent proposed actions (with their
// reason + confidence) so the user can see what dream merged/deduped/flagged.
func (a *dashboardAPI) handleDream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type run struct {
		ID              string `json:"id"`
		StartedAt       string `json:"started_at"`
		FinishedAt      string `json:"finished_at"`
		Status          string `json:"status"`
		MemAnalyzed     int    `json:"memories_analyzed"`
		ActionsProposed int    `json:"actions_proposed"`
		ActionsApplied  int    `json:"actions_applied"`
	}
	var last run
	var sStart, sFin sql.NullString
	_ = a.db.QueryRowContext(ctx,
		`SELECT id, started_at, finished_at, status, memories_analyzed, actions_proposed, actions_applied
		 FROM dream_runs ORDER BY COALESCE(started_at, finished_at, '1970') DESC LIMIT 1`).
		Scan(&last.ID, &sStart, &sFin, &last.Status, &last.MemAnalyzed, &last.ActionsProposed, &last.ActionsApplied)
	last.StartedAt = toISO(sStart.String)
	last.FinishedAt = toISO(sFin.String)

	var totalRuns, totalActions int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dream_runs`).Scan(&totalRuns)
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM dream_actions`).Scan(&totalActions)

	byType := map[string]int{}
	byStatus := map[string]int{}
	rows, err := a.db.QueryContext(ctx, `SELECT COALESCE(action_type,''), COALESCE(status,''), COUNT(*) FROM dream_actions GROUP BY action_type, status`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "dream agg: %v", err)
		return
	}
	for rows.Next() {
		var t, s string
		var c int
		if err := rows.Scan(&t, &s, &c); err != nil {
			continue
		}
		byType[t] += c
		byStatus[s] += c
	}
	rows.Close()

	limit := queryInt(r, "limit", 50, 500)
	rrows, err := a.db.QueryContext(ctx,
		`SELECT action_type, status, confidence, reason, proposed_at, applied_at
		 FROM dream_actions ORDER BY proposed_at DESC LIMIT ?`, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "dream recent: %v", err)
		return
	}
	defer rrows.Close()
	type act struct {
		Action     string  `json:"action_type"`
		Status     string  `json:"status"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
		ProposedAt string  `json:"proposed_at"`
		AppliedAt  string  `json:"applied_at"`
	}
	recent := []act{}
	for rrows.Next() {
		var x act
		var prop, appl sql.NullString
		if err := rrows.Scan(&x.Action, &x.Status, &x.Confidence, &x.Reason, &prop, &appl); err != nil {
			continue
		}
		x.ProposedAt = toISO(prop.String)
		x.AppliedAt = toISO(appl.String)
		recent = append(recent, x)
	}
	if err := rrows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "dream rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{
		"total_runs":    totalRuns,
		"total_actions": totalActions,
		"last_run":      last,
		"by_type":       byType,
		"by_status":     byStatus,
		"recent":        recent,
	})
}

// handleArtifacts serves indexed artifacts (test reports, stack traces…) with
// per-type aggregates and a recent list. type=? filters, limit caps the list.
func (a *dashboardAPI) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type agg struct {
		Count int   `json:"count"`
		Bytes int64 `json:"bytes"`
	}
	byType := map[string]agg{}
	rows, err := a.db.QueryContext(ctx, `SELECT type, COUNT(*), COALESCE(SUM(size_bytes),0) FROM artifacts GROUP BY type`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "artifacts agg: %v", err)
		return
	}
	for rows.Next() {
		var t string
		var ag agg
		if err := rows.Scan(&t, &ag.Count, &ag.Bytes); err != nil {
			continue
		}
		byType[t] = ag
	}
	rows.Close()

	limit := queryInt(r, "limit", 50, 500)
	atype := r.URL.Query().Get("type")
	var rrows *sql.Rows
	if atype != "" {
		rrows, err = a.db.QueryContext(ctx,
			`SELECT type, source_label, source_tool, size_bytes, created_at, ttl_expires_at
			 FROM artifacts WHERE type = ? ORDER BY created_at DESC LIMIT ?`, atype, limit)
	} else {
		rrows, err = a.db.QueryContext(ctx,
			`SELECT type, source_label, source_tool, size_bytes, created_at, ttl_expires_at
			 FROM artifacts ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "artifacts: %v", err)
		return
	}
	defer rrows.Close()
	type art struct {
		Type        string `json:"type"`
		SourceLabel string `json:"source_label"`
		SourceTool  string `json:"source_tool"`
		Bytes       int64  `json:"bytes"`
		CreatedAt   string `json:"created_at"`
		ExpiresAt   string `json:"expires_at"`
	}
	recent := []art{}
	for rrows.Next() {
		var x art
		var created, exp sql.NullString
		if err := rrows.Scan(&x.Type, &x.SourceLabel, &x.SourceTool, &x.Bytes, &created, &exp); err != nil {
			continue
		}
		x.CreatedAt = toISO(created.String)
		x.ExpiresAt = toISO(exp.String)
		recent = append(recent, x)
	}
	if err := rrows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "artifacts rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"by_type": byType, "recent": recent})
}

// handleChunks summarizes the context optimizer's index: chunk count by
// content_type and by source.
func (a *dashboardAPI) handleChunks(w http.ResponseWriter, r *http.Request) {
	byType := map[string]int{}
	bySource := map[string]int{}
	var total int
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT COALESCE(content_type,''), COALESCE(source,''), COUNT(*) FROM content_chunks GROUP BY content_type, source`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "chunks: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t, s string
		var c int
		if err := rows.Scan(&t, &s, &c); err != nil {
			continue
		}
		byType[t] += c
		bySource[s] += c
		total += c
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "chunks rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"total": total, "by_type": byType, "by_source": bySource})
}

// handleEvents surfaces session activity: total events, top tools used, event
// type breakdown, and a recent timeline.
func (a *dashboardAPI) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var total int
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events`).Scan(&total)

	topTools := []map[string]any{}
	rows, err := a.db.QueryContext(ctx, `SELECT tool_name, COUNT(*) c FROM session_events WHERE tool_name != '' GROUP BY tool_name ORDER BY c DESC LIMIT 12`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "events tools: %v", err)
		return
	}
	for rows.Next() {
		var name string
		var c int
		if err := rows.Scan(&name, &c); err != nil {
			continue
		}
		topTools = append(topTools, map[string]any{"tool": name, "count": c})
	}
	rows.Close()

	byType := map[string]int{}
	trows, err := a.db.QueryContext(ctx, `SELECT COALESCE(event_type,''), COUNT(*) FROM session_events GROUP BY event_type`)
	if err == nil {
		for trows.Next() {
			var t string
			var c int
			if err := trows.Scan(&t, &c); err == nil {
				byType[t] = c
			}
		}
		trows.Close()
	}

	limit := queryInt(r, "limit", 50, 500)
	rrows, err := a.db.QueryContext(ctx,
		`SELECT event_type, tool_name, summary, created_at FROM session_events ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "events recent: %v", err)
		return
	}
	defer rrows.Close()
	type ev struct {
		Type      string `json:"event_type"`
		Tool      string `json:"tool_name"`
		Summary   string `json:"summary"`
		CreatedAt string `json:"created_at"`
	}
	recent := []ev{}
	for rrows.Next() {
		var x ev
		var created sql.NullString
		if err := rrows.Scan(&x.Type, &x.Tool, &x.Summary, &created); err != nil {
			continue
		}
		x.CreatedAt = toISO(created.String)
		recent = append(recent, x)
	}
	if err := rrows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "events rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"total": total, "by_type": byType, "top_tools": topTools, "recent": recent})
}

// handleImports lists import history (claude-code, opencode, devclaw, cursor).
func (a *dashboardAPI) handleImports(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT source, path, status, COALESCE(memories_imported,0), COALESCE(entities_imported,0), started_at, finished_at, COALESCE(error,'')
		 FROM imports ORDER BY started_at DESC LIMIT 50`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "imports: %v", err)
		return
	}
	defer rows.Close()
	type imp struct {
		Source    string `json:"source"`
		Path      string `json:"path"`
		Status    string `json:"status"`
		Memories  int    `json:"memories"`
		Entities  int    `json:"entities"`
		StartedAt string `json:"started_at"`
		EndedAt   string `json:"finished_at"`
		Error     string `json:"error"`
	}
	out := []imp{}
	for rows.Next() {
		var x imp
		var started, ended sql.NullString
		if err := rows.Scan(&x.Source, &x.Path, &x.Status, &x.Memories, &x.Entities, &started, &ended, &x.Error); err != nil {
			continue
		}
		x.StartedAt = toISO(started.String)
		x.EndedAt = toISO(ended.String)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, "imports rows: %v", err)
		return
	}
	dashWriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (a *dashboardAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type counts struct {
		Total     int `json:"total"`
		WithEmbed int `json:"with_embedding"`
		SyncDirty int `json:"sync_dirty"`
	}
	var c counts
	// memories.embedding may be NULL/empty for not-yet-embedded rows; treat a
	// non-null BLOB length>0 as covered. sync_dirty is nullable (older rows).
	err := a.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN length(embedding) > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN sync_dirty = 1 THEN 1 ELSE 0 END), 0)
		FROM memories WHERE deleted_at IS NULL`).Scan(&c.Total, &c.WithEmbed, &c.SyncDirty)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "health counts: %v", err)
		return
	}

	var dbBytes int64
	var pageCount, pageSize int64
	_ = a.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount)
	_ = a.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	// Allocated DB size (page_count × page_size), reported as db_bytes. This is
	// the on-disk footprint SQLite has reserved — it ignores WAL/free-list
	// shrinkage until a VACUUM, so treat it as an upper bound on store size.
	dbBytes = pageCount * pageSize

	var syncProjects int
	var watermark sql.NullString
	var lastSync sql.NullTime
	_ = a.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(watermark), MAX(last_sync) FROM sync_state`).
		Scan(&syncProjects, &watermark, &lastSync)

	pct := 0.0
	if c.Total > 0 {
		pct = float64(c.WithEmbed) / float64(c.Total) * 100
	}
	sync := map[string]any{
		"projects":       syncProjects,
		"last_watermark": nullStr(watermark),
		"last_sync_at":   nullTimeStr(lastSync),
	}

	dashWriteJSON(w, http.StatusOK, map[string]any{
		"memories":           c,
		"embedding_coverage": pct,
		"db_bytes":           dbBytes,
		"sync":               sync,
	})
}

// --- small mappers ---

func toMemoryRow(m memory.Memory) memoryRow {
	pid := ""
	if m.ProjectID != nil {
		pid = *m.ProjectID
	}
	return memoryRow{
		ID:        m.ID,
		Category:  m.Category,
		Content:   m.Content,
		Source:    m.Source,
		ProjectID: pid,
		CreatedAt: timeStr(m.CreatedAt),
	}
}

func timeStr(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func nullTimeStr(n sql.NullTime) string {
	if !n.Valid || n.Time.IsZero() {
		return ""
	}
	return n.Time.UTC().Format(time.RFC3339)
}

// dashDateFormats covers the timestamp shapes actually found in the corpus:
// RFC3339 (modern rows), SQLite text with sub-second + zone offsets, and the
// Go time.Time.String() layout some older imports wrote verbatim.
var dashDateFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// toISO normalizes a raw DB timestamp string to RFC3339 so the frontend's
// Date() parser always succeeds; unparseable input is returned unchanged.
func toISO(s string) string {
	if s == "" {
		return ""
	}
	for _, f := range dashDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// keywordStopWords removes common PT/EN fillers so the keyword cloud surfaces
// meaningful terms instead of "de / the / vou / que".
var keywordStopWords = map[string]bool{
	// EN
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"of": true, "to": true, "in": true, "on": true, "for": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "with": true, "as": true,
	"by": true, "at": true, "it": true, "this": true, "that": true, "i": true,
	"you": true, "we": true, "they": true, "not": true, "no": true, "yes": true,
	// PT
	"o": true, "os": true, "de": true, "do": true, "da": true,
	"dos": true, "das": true, "e": true, "ou": true, "um": true, "uma": true,
	"na": true, "nos": true, "nas": true, "em": true, "para": true,
	"com": true, "por": true, "que": true, "se": true, "é": true, "sou": true,
	"vai": true, "vou": true, "tem": true, "têm": true, "mais": true, "como": true,
	"mas": true, "ao": true, "já": true, "não": true, "sim": true, "pra": true,
}

func nullStr(n sql.NullString) string {
	if !n.Valid {
		return ""
	}
	return n.String
}
