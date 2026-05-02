package ctx

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
)

// Store provides CRUD and FTS5 search for ephemeral content chunks and session events.
type Store struct {
	db     *sql.DB
	logger *slog.Logger

	stmtInsertChunk *sql.Stmt
	stmtGetChunk    *sql.Stmt
	stmtExpiredIDs  *sql.Stmt
	stmtTotalSize   *sql.Stmt
	stmtChunksBySrc *sql.Stmt
	stmtInsertEvent *sql.Stmt
	stmtQueryEvents *sql.Stmt
	stmtDeleteEvts  *sql.Stmt
}

// NewStore creates a Store backed by db. If logger is nil, uses slog.Default().
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &Store{db: db, logger: logger}
}

// PrepareStatements pre-compiles frequently used queries. Call after migration.
func (s *Store) PrepareStatements() error {
	var err error

	s.stmtInsertChunk, err = s.db.Prepare(`
		INSERT INTO content_chunks (id, session_id, project_id, source, label, content, metadata, content_type, indexed_at, ttl_hours)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert chunk: %w", err)
	}

	s.stmtGetChunk, err = s.db.Prepare(`
		SELECT id, session_id, project_id, source, label, content, metadata, content_type, indexed_at, ttl_hours
		FROM content_chunks WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare get chunk: %w", err)
	}

	s.stmtExpiredIDs, err = s.db.Prepare(`
		SELECT id FROM content_chunks
		WHERE ttl_hours > 0
		  AND datetime(indexed_at, '+' || ttl_hours || ' hours') <= datetime('now')`)
	if err != nil {
		return fmt.Errorf("prepare expired ids: %w", err)
	}

	s.stmtTotalSize, err = s.db.Prepare(`SELECT COALESCE(SUM(LENGTH(content)), 0) FROM content_chunks`)
	if err != nil {
		return fmt.Errorf("prepare total size: %w", err)
	}

	s.stmtChunksBySrc, err = s.db.Prepare(`
		SELECT id, session_id, project_id, source, label, content, metadata, content_type, indexed_at, ttl_hours
		FROM content_chunks WHERE source = ?`)
	if err != nil {
		return fmt.Errorf("prepare chunks by source: %w", err)
	}

	s.stmtInsertEvent, err = s.db.Prepare(`
		INSERT INTO session_events (id, session_id, project_id, event_type, priority, tool_name, summary, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert event: %w", err)
	}

	s.stmtQueryEvents, err = s.db.Prepare(`
		SELECT id, session_id, project_id, event_type, priority, tool_name, summary, metadata, created_at
		FROM session_events WHERE session_id = ?
		ORDER BY created_at DESC LIMIT ?`)
	if err != nil {
		return fmt.Errorf("prepare query events: %w", err)
	}

	s.stmtDeleteEvts, err = s.db.Prepare(`DELETE FROM session_events WHERE session_id = ?`)
	if err != nil {
		return fmt.Errorf("prepare delete events: %w", err)
	}

	return nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// InsertChunk generates a UUID and inserts chunk into content_chunks.
func (s *Store) InsertChunk(ctx context.Context, chunk *Chunk) error {
	if chunk.ID == "" {
		chunk.ID = newID()
	}
	_, err := s.stmtInsertChunk.ExecContext(ctx,
		chunk.ID, chunk.SessionID, chunk.ProjectID, chunk.Source, chunk.Label,
		chunk.Content, chunk.Metadata, chunk.ContentType,
		chunk.IndexedAt, chunk.TTLHours,
	)
	if err != nil {
		return fmt.Errorf("insert chunk: %w", err)
	}
	return nil
}

// GetChunk retrieves a single chunk by ID.
func (s *Store) GetChunk(ctx context.Context, id string) (*Chunk, error) {
	row := s.stmtGetChunk.QueryRowContext(ctx, id)
	var c Chunk
	err := row.Scan(
		&c.ID, &c.SessionID, &c.ProjectID, &c.Source, &c.Label,
		&c.Content, &c.Metadata, &c.ContentType,
		&c.IndexedAt, &c.TTLHours,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get chunk %s: %w", id, err)
	}
	return &c, nil
}

// DeleteChunks removes chunks by IDs in a single transaction.
func (s *Store) DeleteChunks(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`DELETE FROM content_chunks WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("prepare delete: %w", err)
	}
	defer stmt.Close()

	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("delete chunk %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// GetExpiredChunkIDs returns IDs of chunks whose TTL has elapsed.
func (s *Store) GetExpiredChunkIDs(ctx context.Context) ([]string, error) {
	rows, err := s.stmtExpiredIDs.QueryContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("query expired: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetTotalSize returns the sum of length(content) across all chunks.
func (s *Store) GetTotalSize(ctx context.Context) (int64, error) {
	var size int64
	err := s.stmtTotalSize.QueryRowContext(ctx).Scan(&size)
	if err != nil {
		return 0, fmt.Errorf("total size: %w", err)
	}
	return size, nil
}

// GetChunksBySource returns all chunks matching the given source label.
func (s *Store) GetChunksBySource(ctx context.Context, source string) ([]Chunk, error) {
	rows, err := s.stmtChunksBySrc.QueryContext(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("chunks by source: %w", err)
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var c Chunk
		if err := rows.Scan(
			&c.ID, &c.SessionID, &c.ProjectID, &c.Source, &c.Label,
			&c.Content, &c.Metadata, &c.ContentType,
			&c.IndexedAt, &c.TTLHours,
		); err != nil {
			return nil, fmt.Errorf("scan chunk: %w", err)
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}

// SearchChunks performs FTS5 trigram search with optional content_type and source filters.
// User query terms are wrapped in double quotes for trigram matching.
// BM25 rank (negative) is normalized to a positive score: 1.0 / (1.0 + -rank).
func (s *Store) SearchChunks(ctx context.Context, query string, maxResults int, contentType string, source string, projectID string) ([]ContentSearchResult, error) {
	if maxResults <= 0 {
		maxResults = 20
	}

	terms := strings.Fields(query)
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	matchExpr := strings.Join(quoted, " ")

	var qb strings.Builder
	qb.WriteString(`SELECT cc.id, cc.label, cc.source, cc.content, fts.rank
		FROM content_chunks_fts fts
		JOIN content_chunks cc ON fts.rowid = cc.rowid
		WHERE content_chunks_fts MATCH ?`)

	args := []any{matchExpr}

	if projectID != "" {
		qb.WriteString(" AND cc.project_id = ?")
		args = append(args, projectID)
	}
	if contentType != "" {
		qb.WriteString(" AND cc.content_type = ?")
		args = append(args, contentType)
	}
	if source != "" {
		qb.WriteString(" AND cc.source LIKE '%' || ? || '%'")
		args = append(args, source)
	}

	qb.WriteString(" ORDER BY rank LIMIT ?")
	args = append(args, maxResults)

	rows, err := s.db.QueryContext(ctx, qb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	var results []ContentSearchResult
	for rows.Next() {
		var id, label, src, ct string
		var rank float64
		if err := rows.Scan(&id, &label, &src, &ct, &rank); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}

		score := 0.0
		if rank < 0 {
			score = 1.0 / (1.0 + -rank)
		}

		results = append(results, ContentSearchResult{
			ChunkID: id,
			Label:   label,
			Source:  src,
			Snippet: ct,
			Score:   score,
		})
	}
	return results, rows.Err()
}

// InsertEvent generates a UUID and inserts a session event.
func (s *Store) InsertEvent(ctx context.Context, event *SessionEvent) error {
	if event.ID == "" {
		event.ID = newID()
	}
	_, err := s.stmtInsertEvent.ExecContext(ctx,
		event.ID, event.SessionID, event.ProjectID, event.EventType, event.Priority,
		event.ToolName, event.Summary, event.Metadata, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// QueryEvents retrieves events for a session, ordered by created_at DESC.
func (s *Store) QueryEvents(ctx context.Context, sessionID string, limit int) ([]SessionEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.stmtQueryEvents.QueryContext(ctx, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []SessionEvent
	for rows.Next() {
		var e SessionEvent
		if err := rows.Scan(
			&e.ID, &e.SessionID, &e.ProjectID, &e.EventType, &e.Priority,
			&e.ToolName, &e.Summary, &e.Metadata, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// DeleteEventsBySession removes all events for a given session.
func (s *Store) DeleteEventsBySession(ctx context.Context, sessionID string) error {
	_, err := s.stmtDeleteEvts.ExecContext(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("delete events for session %s: %w", sessionID, err)
	}
	return nil
}
