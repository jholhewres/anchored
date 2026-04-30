package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type SQLiteStore struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewSQLiteStore(dbPath string, logger *slog.Logger) (*SQLiteStore, error) {
	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=30000&_txlock=immediate", dbPath)

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

	return &SQLiteStore{db: db, logger: logger}, nil
}

func (s *SQLiteStore) DB() *sql.DB { return s.db }

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *SQLiteStore) Save(ctx context.Context, m Memory) error {
	now := time.Now().UTC()

	if m.ID == "" {
		m.ID = newUUID()
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	var keywordsJSON any
	if m.Keywords != nil {
		b, err := json.Marshal(m.Keywords)
		if err != nil {
			return fmt.Errorf("marshal keywords: %w", err)
		}
		keywordsJSON = string(b)
	}

	var metadataJSON any
	if m.Metadata != nil {
		b, err := json.Marshal(m.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		metadataJSON = string(b)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, project_id, category, content, keywords, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			project_id = excluded.project_id,
			category = excluded.category,
			content = excluded.content,
			keywords = excluded.keywords,
			source = excluded.source,
			source_id = excluded.source_id,
			updated_at = excluded.updated_at,
			metadata = excluded.metadata`,
		m.ID, m.ProjectID, m.Category, m.Content, keywordsJSON, m.Source, m.SourceID,
		m.CreatedAt, m.UpdatedAt, m.AccessCount, m.LastAccessed, metadataJSON,
	)
	if err != nil {
		return fmt.Errorf("save memory: %w", err)
	}

	return nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (*Memory, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_id, category, content, keywords, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata
		 FROM memories WHERE id = ?`, id,
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

func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 20
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.project_id, m.category, m.content, m.keywords, m.source, m.source_id,
		        m.created_at, m.updated_at, m.access_count, m.last_accessed_at, m.metadata,
		        bm25(memories_fts) AS rank
		 FROM memories_fts fts
		 JOIN memories m ON m.rowid = fts.rowid
		 WHERE memories_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		query, maxResults,
	)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var m Memory
		var rank float64
		var keywordsStr, metadataStr sql.NullString
		var projectID, sourceID sql.NullString
		var lastAccessedTime sql.NullTime

		err := rows.Scan(
			&m.ID, &projectID, &m.Category, &m.Content, &keywordsStr, &m.Source, &sourceID,
			&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessedTime, &metadataStr,
			&rank,
		)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
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

func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete memory %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) List(ctx context.Context, opts ListOptions) ([]Memory, error) {
	qb := strings.Builder{}
	qb.WriteString(`SELECT id, project_id, category, content, keywords, source, source_id, created_at, updated_at, access_count, last_accessed_at, metadata FROM memories`)
	var args []any
	var conditions []string

	if opts.Category != "" {
		conditions = append(conditions, "category = ?")
		args = append(args, opts.Category)
	}
	if opts.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		args = append(args, opts.ProjectID)
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

	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memories").Scan(&stats.TotalMemories)
	if err != nil {
		return nil, fmt.Errorf("count memories: %w", err)
	}

	catRows, err := s.db.QueryContext(ctx, "SELECT category, COUNT(*) FROM memories GROUP BY category")
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

	projRows, err := s.db.QueryContext(ctx, "SELECT project_id, COUNT(*) FROM memories WHERE project_id IS NOT NULL GROUP BY project_id")
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

	return stats, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func scanMemory(row *sql.Row) (*Memory, error) {
	var m Memory
	var keywordsStr, metadataStr sql.NullString
	var projectID, sourceID sql.NullString
	var lastAccessed sql.NullTime

	err := row.Scan(
		&m.ID, &projectID, &m.Category, &m.Content, &keywordsStr, &m.Source, &sourceID,
		&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessed, &metadataStr,
	)
	if err != nil {
		return nil, err
	}

	m.ProjectID = nilIfNull(projectID)
	m.SourceID = nilIfNull(sourceID)
	m.Keywords = unmarshalKeywords(keywordsStr)
	m.LastAccessed = nilTimeIfZero(lastAccessed)
	if metadataStr.Valid {
		json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
	}

	return &m, nil
}

func scanMemoryRow(rows *sql.Rows) (*Memory, error) {
	var m Memory
	var keywordsStr, metadataStr sql.NullString
	var projectID, sourceID sql.NullString
	var lastAccessed sql.NullTime

	err := rows.Scan(
		&m.ID, &projectID, &m.Category, &m.Content, &keywordsStr, &m.Source, &sourceID,
		&m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessed, &metadataStr,
	)
	if err != nil {
		return nil, fmt.Errorf("scan memory row: %w", err)
	}

	m.ProjectID = nilIfNull(projectID)
	m.SourceID = nilIfNull(sourceID)
	m.Keywords = unmarshalKeywords(keywordsStr)
	m.LastAccessed = nilTimeIfZero(lastAccessed)
	if metadataStr.Valid {
		json.Unmarshal([]byte(metadataStr.String), &m.Metadata)
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
