package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
)

type ImportRecord struct {
	ID               string
	Source           string
	Path             string
	MemoriesImported int
	EntitiesImported int
	Status           string
	StartedAt        *time.Time
	FinishedAt       *time.Time
	Error            string
}

type SQLiteStore struct {
	db                    *sql.DB
	cache                 *VectorCache
	logger                *slog.Logger
	now                   func() time.Time
	embeddingGenerationMu sync.Mutex
}

func NewSQLiteStore(dbPath string, logger *slog.Logger) (*SQLiteStore, error) {
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=30000&_txlock=immediate&_foreign_keys=on", dbPath)

	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", dbPath, err)
	}

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)

	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if logger == nil {
		logger = slog.Default()
	}

	cache := NewVectorCache(logger)
	store := &SQLiteStore{db: db, cache: cache, logger: logger, now: time.Now}
	if active, activeErr := store.ActiveEmbeddingGeneration(context.Background()); activeErr != nil {
		logger.Warn("active embedding generation lookup failed", "error", activeErr)
	} else if active != nil {
		if vectors, loadErr := store.LoadEmbeddingGeneration(context.Background(), active.ID); loadErr != nil {
			logger.Warn("active embedding generation load failed", "generation", active.ID, "error", loadErr)
		} else {
			cache.Replace(vectors)
			logger.Info("active embedding generation loaded", "generation", active.ID, "count", len(vectors))
		}
	}

	return store, nil
}

func (s *SQLiteStore) DB() *sql.DB               { return s.db }
func (s *SQLiteStore) VectorCache() *VectorCache { return s.cache }

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail on a healthy system, but ignoring the
		// error would silently return an all-zero (non-unique) id.
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// contentHash is the exact-dedup key. It hashes content verbatim (no
// normalization) ON PURPOSE: the same hash is sent in the sync payload
// (SyncMemory.ContentHash) and the server + older clients dedup on it, so the
// algorithm must stay byte-identical across versions for backwards
// compatibility. Trivial case/whitespace variants are folded by the
// near-duplicate merge on the save path instead (Service.findNearDuplicate),
// which keeps the sync contract stable.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func (s *SQLiteStore) Save(ctx context.Context, m Memory) error {
	_, err := s.SaveTemporal(ctx, m, TemporalWriteOptions{})
	return err
}

func (s *SQLiteStore) nowUTC() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (*Memory, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, category, content, content_hash, keywords, embedding, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata, sync_dirty, sync_origin, author, remote_project_key
		 FROM memories WHERE id = ? AND deleted_at IS NULL`, id,
	)

	m, err := scanMemory(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get memory %s: %w", id, err)
	}
	return m, nil
}

// isFTSSyntaxError reports whether err is an FTS5 query-parse failure (as
// opposed to a real I/O/DB error), so Search can retry with a sanitized query.
func isFTSSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// FTS5 surfaces a malformed MATCH expression in several shapes: a bare
	// "fts5: syntax error", a "no such column: X" when a token contains a colon
	// (col:term filter), an unterminated quoted string, or an unknown special
	// query. In this Search the only dynamic input is the MATCH argument and the
	// surrounding SQL is static/valid, so any of these means the query — not the
	// schema — is at fault.
	for _, needle := range []string{"fts5", "syntax error", "no such column", "unterminated", "unknown special query", "malformed match"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// safeFTSOr reduces an arbitrary string to a safe FTS5 MATCH expression: each
// alphanumeric token is double-quoted (so it's a literal, immune to operators)
// and the tokens are OR-joined. Returns "" when the input has no usable tokens.
func safeFTSOr(query string) string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	quoted := make([]string, 0, len(fields))
	for _, f := range fields {
		quoted = append(quoted, `"`+f+`"`)
	}
	return strings.Join(quoted, " OR ")
}

func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 20
	}

	qb := strings.Builder{}
	qb.WriteString(`SELECT m.id, m.project_id, m.category, m.content, m.keywords, m.source, m.source_id,
		        m.created_at, m.updated_at, m.access_count, m.last_accessed_at, m.metadata,
		        bm25(memories_fts) AS rank
		 FROM memories_fts fts
		 JOIN memories m ON m.rowid = fts.rowid
		 WHERE memories_fts MATCH ? AND m.deleted_at IS NULL`)
	args := []any{query}

	if opts.Category != "" {
		qb.WriteString(" AND m.category = ?")
		args = append(args, opts.Category)
	}
	if opts.ProjectID != "" {
		qb.WriteString(" AND m.project_id = ?")
		args = append(args, opts.ProjectID)
	}

	qb.WriteString(" ORDER BY rank LIMIT ?")
	args = append(args, maxResults)
	sqlStr := qb.String()

	// exec runs the prepared query with a given MATCH expression and fully
	// drains the rows. FTS5 (via mattn) surfaces a malformed MATCH lazily during
	// iteration, not at QueryContext, so the error must be caught here for the
	// crash-safe retry to work.
	exec := func(matchExpr string) ([]SearchResult, error) {
		// Build a private args slice (don't mutate the shared one in place) so
		// the closure stays safe under any future concurrent use.
		execArgs := append([]any{matchExpr}, args[1:]...)
		rows, err := s.db.QueryContext(ctx, sqlStr, execArgs...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var results []SearchResult
		for rows.Next() {
			var m Memory
			var rank float64
			var keywordsStr, metadataStr sql.NullString
			var projectID, sourceID sql.NullString
			var lastAccessedTime sql.NullTime

			if err := rows.Scan(
				&m.ID, &projectID, &m.Category, &m.Content, &keywordsStr, &m.Source, &sourceID,
				&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessedTime, &metadataStr,
				&rank,
			); err != nil {
				return nil, err
			}

			m.ProjectID = nilIfNull(projectID)
			m.SourceID = nilIfNull(sourceID)
			m.Keywords = unmarshalKeywords(keywordsStr)
			m.LastAccessed = nilTimeIfZero(lastAccessedTime)
			if metadataStr.Valid {
				json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
			}

			// BM25 rank is negative (more negative = better match).
			// Negate and normalize to positive [0,1] range for hybrid fusion.
			score := 0.0
			if rank < 0 {
				score = 1.0 / (1.0 + -rank)
			}
			results = append(results, SearchResult{Memory: m, Score: score})
		}
		return results, rows.Err()
	}

	results, err := exec(query)
	if err != nil && isFTSSyntaxError(err) {
		// Crash-safe: the caller passed a raw query with FTS5 metacharacters
		// (punctuation, a bare colon, unbalanced quotes) that isn't a valid MATCH
		// expression. Retry once with the query reduced to a safe OR of quoted
		// tokens rather than failing the search.
		safe := safeFTSOr(query)
		if safe == "" {
			return nil, nil
		}
		results, err = exec(safe)
	}
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return results, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete memory %s: %w", id, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_processing_jobs WHERE memory_id = ?", id); err != nil {
		return fmt.Errorf("delete memory processing jobs %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM remote_outbox WHERE memory_id = ?", id); err != nil {
		return fmt.Errorf("delete memory remote outbox %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memory_revisions WHERE memory_id = ?", id); err != nil {
		return fmt.Errorf("delete memory revisions %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete memory %s: %w", id, err)
	}
	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("delete memory %s: %w", id, err)
	}
	s.cache.Remove(id)
	return nil
}

func (s *SQLiteStore) List(ctx context.Context, opts ListOptions) ([]Memory, error) {
	qb := strings.Builder{}
	qb.WriteString(`SELECT id, project_id, category, content, content_hash, keywords, embedding, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata, sync_dirty, sync_origin, author, remote_project_key FROM memories`)
	var args []any
	var conditions []string

	if !opts.IncludeDeleted {
		conditions = append(conditions, "deleted_at IS NULL")
	}

	switch {
	case len(opts.Categories) > 0:
		// Build a `category IN (?, ?, ...)` clause. ?-placeholders only —
		// driver handles escaping; never inline category strings.
		marks := make([]string, len(opts.Categories))
		for i, c := range opts.Categories {
			marks[i] = "?"
			args = append(args, c)
		}
		conditions = append(conditions, "category IN ("+strings.Join(marks, ",")+")")
	case opts.Category != "":
		conditions = append(conditions, "category = ?")
		args = append(args, opts.Category)
	}
	if opts.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		args = append(args, opts.ProjectID)
	}
	if opts.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, opts.Source)
	}

	if len(conditions) > 0 {
		qb.WriteString(" WHERE ")
		qb.WriteString(strings.Join(conditions, " AND "))
	}

	qb.WriteString(" ORDER BY created_at DESC")

	if opts.Limit > 0 {
		qb.WriteString(" LIMIT ?")
		args = append(args, opts.Limit)
	}
	if opts.Offset > 0 {
		qb.WriteString(" OFFSET ?")
		args = append(args, opts.Offset)
	}

	rows, err := s.db.QueryContext(ctx, qb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		m, err := scanMemoryRow(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, *m)
	}

	return memories, rows.Err()
}

func (s *SQLiteStore) Stats(ctx context.Context) (*StoreStats, error) {
	stats := &StoreStats{
		ByCategory: make(map[string]int),
		ByProject:  make(map[string]int),
	}

	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL").Scan(&stats.TotalMemories)
	if err != nil {
		return nil, fmt.Errorf("count memories: %w", err)
	}

	catRows, err := s.db.QueryContext(ctx, "SELECT category, COUNT(*) FROM memories WHERE deleted_at IS NULL GROUP BY category")
	if err != nil {
		return nil, fmt.Errorf("stats by category: %w", err)
	}
	defer catRows.Close()
	for catRows.Next() {
		var cat string
		var count int
		if err := catRows.Scan(&cat, &count); err != nil {
			return nil, err
		}
		stats.ByCategory[cat] = count
	}
	if err := catRows.Err(); err != nil {
		return nil, fmt.Errorf("stats by category: %w", err)
	}

	projRows, err := s.db.QueryContext(ctx, "SELECT project_id, COUNT(*) FROM memories WHERE project_id IS NOT NULL AND deleted_at IS NULL GROUP BY project_id")
	if err != nil {
		return nil, fmt.Errorf("stats by project: %w", err)
	}
	defer projRows.Close()
	for projRows.Next() {
		var proj string
		var count int
		if err := projRows.Scan(&proj, &count); err != nil {
			return nil, err
		}
		stats.ByProject[proj] = count
	}
	if err := projRows.Err(); err != nil {
		return nil, fmt.Errorf("stats by project: %w", err)
	}

	return stats, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) CreateImport(id, source, path string) error {
	_, err := s.db.Exec(
		`INSERT INTO imports (id, source, path, status, started_at) VALUES (?, ?, ?, 'running', CURRENT_TIMESTAMP)`,
		id, source, path,
	)
	return err
}

func (s *SQLiteStore) UpdateImport(id, status string, memoriesImported int, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE imports SET status = ?, memories_imported = ?, finished_at = CURRENT_TIMESTAMP, error = ? WHERE id = ?`,
		status, memoriesImported, errMsg, id,
	)
	return err
}

func (s *SQLiteStore) GetLastImport(source string) (*ImportRecord, error) {
	row := s.db.QueryRow(
		`SELECT id, source, path, memories_imported, entities_imported, status, started_at, finished_at, error
		 FROM imports WHERE source = ? ORDER BY started_at DESC LIMIT 1`, source,
	)
	var r ImportRecord
	var entities int
	var startedAt, finishedAt sql.NullTime
	var errMsg sql.NullString
	err := row.Scan(&r.ID, &r.Source, &r.Path, &r.MemoriesImported, &entities, &r.Status, &startedAt, &finishedAt, &errMsg)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.EntitiesImported = entities
	if startedAt.Valid {
		r.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	if errMsg.Valid {
		r.Error = errMsg.String
	}
	return &r, nil
}

func (s *SQLiteStore) UpdateEmbedding(ctx context.Context, id string, embedding []float32) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE memories SET embedding = ? WHERE id = ?",
		float32sToBlob(embedding), id,
	)
	if err != nil {
		return fmt.Errorf("update embedding for %s: %w", id, err)
	}
	s.refreshVectorCache(ctx, id)
	return nil
}

// refreshVectorCache projects only the active, identified semantic generation
// when one exists. Legacy embedding blobs remain readable through Store APIs,
// but they cannot contaminate generation-aware search in-process.
func (s *SQLiteStore) refreshVectorCache(ctx context.Context, id string) {
	if s.cache == nil || id == "" {
		return
	}
	active, err := s.ActiveEmbeddingGeneration(ctx)
	if err != nil {
		s.cache.Remove(id)
		return
	}

	var blob []byte
	if active != nil {
		err = s.db.QueryRowContext(ctx, `
			SELECT v.embedding
			FROM memories m
			JOIN memory_embedding_vectors v
			  ON v.revision_id = m.current_revision_id
			 AND v.generation_id = ?
			 AND v.purpose = 'document'
			WHERE m.id = ? AND m.deleted_at IS NULL`,
			active.ID, id,
		).Scan(&blob)
	} else {
		err = s.db.QueryRowContext(ctx, `
			SELECT embedding
			FROM memories
			WHERE id = ? AND deleted_at IS NULL AND embedding IS NOT NULL`,
			id,
		).Scan(&blob)
	}
	if err != nil || len(blob) == 0 {
		s.cache.Remove(id)
		return
	}
	vector, err := blobToFloat32s(blob)
	if err != nil || len(vector) == 0 {
		s.cache.Remove(id)
		return
	}
	s.cache.Put(id, vector)
}

func scanMemory(row *sql.Row) (*Memory, error) {
	var m Memory
	var keywordsStr, metadataStr sql.NullString
	var projectID, sourceID sql.NullString
	var lastAccessed sql.NullTime
	var embeddingBlob []byte
	var contentHash sql.NullString
	var syncOrigin, author, remoteProjectKey sql.NullString

	err := row.Scan(
		&m.ID, &projectID, &m.Category, &m.Content, &contentHash, &keywordsStr, &embeddingBlob, &m.Source, &sourceID,
		&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessed, &metadataStr,
		&m.SyncDirty, &syncOrigin, &author, &remoteProjectKey,
	)
	if err != nil {
		return nil, err
	}

	m.ProjectID = nilIfNull(projectID)
	m.SourceID = nilIfNull(sourceID)
	m.ContentHash = contentHash.String
	m.Keywords = unmarshalKeywords(keywordsStr)
	m.LastAccessed = nilTimeIfZero(lastAccessed)
	m.SyncOrigin = syncOrigin.String
	m.Author = nilIfNull(author)
	m.RemoteProjectKey = nilIfNull(remoteProjectKey)
	if metadataStr.Valid {
		json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
	}
	if len(embeddingBlob) > 0 {
		m.Embedding, _ = blobToFloat32s(embeddingBlob)
	}

	return &m, nil
}

func scanMemoryRow(rows *sql.Rows) (*Memory, error) {
	var m Memory
	var keywordsStr, metadataStr sql.NullString
	var projectID, sourceID sql.NullString
	var lastAccessed sql.NullTime
	var embeddingBlob []byte
	var contentHash sql.NullString
	// source is nullable in the schema; rows inserted outside Save (raw
	// tooling, older imports) can carry NULL and must not break a full-corpus
	// scan like `curation reconcile`.
	var source sql.NullString
	var syncOrigin, author, remoteProjectKey sql.NullString

	err := rows.Scan(
		&m.ID, &projectID, &m.Category, &m.Content, &contentHash, &keywordsStr, &embeddingBlob, &source, &sourceID,
		&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessed, &metadataStr,
		&m.SyncDirty, &syncOrigin, &author, &remoteProjectKey,
	)
	if err != nil {
		return nil, fmt.Errorf("scan memory row: %w", err)
	}
	m.Source = source.String

	m.ProjectID = nilIfNull(projectID)
	m.SourceID = nilIfNull(sourceID)
	m.ContentHash = contentHash.String
	m.Keywords = unmarshalKeywords(keywordsStr)
	m.LastAccessed = nilTimeIfZero(lastAccessed)
	m.SyncOrigin = syncOrigin.String
	m.Author = nilIfNull(author)
	m.RemoteProjectKey = nilIfNull(remoteProjectKey)
	if metadataStr.Valid {
		json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
	}
	if len(embeddingBlob) > 0 {
		m.Embedding, _ = blobToFloat32s(embeddingBlob)
	}

	return &m, nil
}

func nilIfNull(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	return &ns.String
}

func nilTimeIfZero(nt sql.NullTime) *time.Time {
	if !nt.Valid {
		return nil
	}
	return &nt.Time
}

func unmarshalKeywords(ns sql.NullString) []string {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var kw []string
	json.Unmarshal([]byte(ns.String), &kw)
	return kw
}

func (s *SQLiteStore) CountWithoutEmbedding(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories WHERE (embedding IS NULL OR LENGTH(embedding) = 0) AND deleted_at IS NULL").Scan(&count)
	return count, err
}

func (s *SQLiteStore) ListWithoutEmbedding(ctx context.Context, limit int) ([]Memory, error) {
	q := `SELECT id, project_id, category, content, content_hash, keywords, embedding, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata, sync_dirty, sync_origin, author, remote_project_key FROM memories WHERE (embedding IS NULL OR LENGTH(embedding) = 0) AND deleted_at IS NULL ORDER BY created_at ASC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list without embedding: %w", err)
	}
	defer rows.Close()

	var memories []Memory
	for rows.Next() {
		m, err := scanMemoryRow(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, *m)
	}
	return memories, rows.Err()
}

func (s *SQLiteStore) Update(ctx context.Context, id string, content string, category string) error {
	_, err := s.UpdateTemporal(ctx, id, TemporalMutation{
		Content:        &content,
		Category:       &category,
		ClearEmbedding: true,
		ExpectedState:  TemporalStateActive,
	}, TemporalWriteOptions{})
	return err
}

func (s *SQLiteStore) UpdateMetadata(ctx context.Context, id string, metadata any) error {
	return s.updateMetadataTemporal(ctx, id, metadata)
}

func (s *SQLiteStore) SoftDelete(ctx context.Context, id string) error {
	_, err := s.UpdateTemporal(ctx, id, TemporalMutation{
		ExpectedState: TemporalStateActive,
	}, TemporalWriteOptions{Mode: TemporalTombstone})
	return err
}

// Restore undoes a soft-delete (deleted_at -> NULL). It is the inverse of
// SoftDelete: same cache invalidation and only touches rows that are currently
// deleted, so re-restoring an active memory is a no-op.
func (s *SQLiteStore) Restore(ctx context.Context, id string) error {
	_, err := s.UpdateTemporal(ctx, id, TemporalMutation{
		ExpectedState: TemporalStateTombstone,
	}, TemporalWriteOptions{Mode: TemporalSupersede})
	return err
}

func (s *SQLiteStore) DeleteByScope(ctx context.Context, opts DeleteScopeOptions) (int, error) {
	var conditions []string
	var args []any

	if opts.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		args = append(args, opts.ProjectID)
	}
	if opts.Category != "" {
		conditions = append(conditions, "category = ?")
		args = append(args, opts.Category)
	}
	if opts.Source != "" {
		conditions = append(conditions, "source = ?")
		args = append(args, opts.Source)
	}

	if len(conditions) == 0 {
		return 0, fmt.Errorf("at least one scope condition is required")
	}

	if opts.Hard {
		where := strings.Join(conditions, " AND ")
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM memories WHERE "+where, args...)
		if err != nil {
			return 0, fmt.Errorf("list hard-delete scope: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin hard delete by scope: %w", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM memory_processing_jobs WHERE memory_id IN (SELECT id FROM memories WHERE "+where+")",
			args...,
		); err != nil {
			return 0, fmt.Errorf("hard delete processing jobs by scope: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM remote_outbox WHERE memory_id IN (SELECT id FROM memories WHERE "+where+")",
			args...,
		); err != nil {
			return 0, fmt.Errorf("hard delete remote outbox by scope: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM memory_revisions WHERE memory_id IN (SELECT id FROM memories WHERE "+where+")",
			args...,
		); err != nil {
			return 0, fmt.Errorf("hard delete revision history by scope: %w", err)
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE "+where, args...)
		if err != nil {
			return 0, fmt.Errorf("hard delete by scope: %w", err)
		}
		n, _ := result.RowsAffected()
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit hard delete by scope: %w", err)
		}
		for _, id := range ids {
			s.cache.Remove(id)
		}
		return int(n), nil
	}

	return s.softDeleteByScopeTemporal(ctx, strings.Join(conditions, " AND "), args)
}

func (s *SQLiteStore) FindByContentHash(ctx context.Context, hash string, projectID *string) (*Memory, error) {
	var row *sql.Row
	if projectID != nil {
		row = s.db.QueryRowContext(ctx,
			`SELECT id, project_id, category, content, content_hash, keywords, embedding, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata, sync_dirty, sync_origin, author, remote_project_key
			 FROM memories WHERE content_hash = ? AND project_id = ? AND deleted_at IS NULL`,
			hash, *projectID,
		)
	} else {
		row = s.db.QueryRowContext(ctx,
			`SELECT id, project_id, category, content, content_hash, keywords, embedding, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata, sync_dirty, sync_origin, author, remote_project_key
			 FROM memories WHERE content_hash = ? AND project_id IS NULL AND deleted_at IS NULL`,
			hash,
		)
	}

	m, err := scanMemory(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("find by content hash: %w", err)
	}
	return m, nil
}

func (s *SQLiteStore) BackfillContentHash(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, content FROM memories WHERE content_hash IS NULL OR content_hash = ''",
	)
	if err != nil {
		return 0, fmt.Errorf("backfill content hash query: %w", err)
	}
	defer rows.Close()

	var total int
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			continue
		}
		hash := contentHash(content)
		if _, err := s.db.ExecContext(ctx,
			"UPDATE memories SET content_hash = ? WHERE id = ?", hash, id,
		); err != nil {
			s.logger.Warn("backfill content hash failed", "id", id, "error", err)
			continue
		}
		total++
	}
	return total, rows.Err()
}
