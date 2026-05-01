package stack

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

const defaultEssentialCacheTTL = 6 * time.Hour
const essentialSchemaVersion = 1

type essentialMemory struct {
	Category   string
	Content    string
	AccessCount int
	CreatedAt  time.Time
}

type EssentialLayer struct {
	store       DBAccessor
	cacheTTL    time.Duration
	logger      *slog.Logger
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

type DBAccessor interface {
	DB() *sql.DB
}

func NewEssentialLayer(store DBAccessor, logger *slog.Logger) *EssentialLayer {
	if logger == nil {
		logger = slog.Default()
	}
	return &EssentialLayer{
		store:    store,
		cacheTTL: defaultEssentialCacheTTL,
		logger:   logger,
	}
}

func (l *EssentialLayer) Render(projectID string) string {
	if projectID == "" {
		return ""
	}

	if story := l.loadCache(projectID); story != "" {
		l.cacheHits.Add(1)
		return story
	}

	l.cacheMisses.Add(1)

	memories, err := l.queryTopMemories(projectID)
	if err != nil {
		l.logger.Warn("essential: query memories failed", "error", err, "project", projectID)
		return ""
	}
	if len(memories) == 0 {
		return ""
	}

	story := l.buildMarkdown(memories)

	if err := l.saveCache(projectID, story); err != nil {
		l.logger.Warn("essential: save cache failed", "error", err, "project", projectID)
	}

	return story
}

func (l *EssentialLayer) Invalidate(projectID string) {
	if projectID == "" {
		return
	}
	_, err := l.store.DB().Exec("DELETE FROM essential_stories WHERE project_id = ?", projectID)
	if err != nil {
		l.logger.Warn("essential: invalidate cache failed", "error", err, "project", projectID)
	}
}

// CacheHits returns the number of cache hits since creation.
func (l *EssentialLayer) CacheHits() int64 {
	return l.cacheHits.Load()
}

// CacheMisses returns the number of cache misses since creation.
func (l *EssentialLayer) CacheMisses() int64 {
	return l.cacheMisses.Load()
}

func (l *EssentialLayer) loadCache(projectID string) string {
	var story string
	var generatedAt time.Time

	err := l.store.DB().QueryRow(
		"SELECT story, generated_at FROM essential_stories WHERE project_id = ?",
		projectID,
	).Scan(&story, &generatedAt)

	if err != nil {
		if err != sql.ErrNoRows {
			l.logger.Warn("essential: cache read failed", "error", err, "project", projectID)
		}
		return ""
	}

	if time.Since(generatedAt) > l.cacheTTL {
		return ""
	}

	return story
}

func (l *EssentialLayer) saveCache(projectID, story string) error {
	now := time.Now().UTC()
	_, err := l.store.DB().Exec(
		`INSERT INTO essential_stories (project_id, story, generated_at, bytes, schema_version)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET
			story = excluded.story,
			generated_at = excluded.generated_at,
			bytes = excluded.bytes,
			schema_version = excluded.schema_version`,
		projectID, story, now, len(story), essentialSchemaVersion,
	)
	return err
}

func (l *EssentialLayer) queryTopMemories(projectID string) ([]essentialMemory, error) {
	rows, err := l.store.DB().Query(
		`SELECT category, content, access_count, created_at
		 FROM memories
		 WHERE project_id = ?
		 ORDER BY access_count DESC, created_at DESC
		 LIMIT 20`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("query top memories: %w", err)
	}
	defer rows.Close()

	var memories []essentialMemory
	for rows.Next() {
		var m essentialMemory
		if err := rows.Scan(&m.Category, &m.Content, &m.AccessCount, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		memories = append(memories, m)
	}
	return memories, rows.Err()
}

func (l *EssentialLayer) buildMarkdown(memories []essentialMemory) string {
	var facts, decisions, events, preferences []essentialMemory

	for _, m := range memories {
		switch m.Category {
		case "fact":
			facts = append(facts, m)
		case "decision":
			decisions = append(decisions, m)
		case "event":
			events = append(events, m)
		case "preference":
			preferences = append(preferences, m)
		}
	}

	var sb strings.Builder
	sb.WriteString("## Project Context\n")

	keyItems := mergeAndLimit(facts, decisions, 5)
	if len(keyItems) > 0 {
		sb.WriteString("\n### Key Facts & Decisions\n")
		for _, m := range keyItems {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Content))
		}
	}

	if len(events) > 0 {
		sb.WriteString("\n### Recent Events\n")
		for _, m := range events[:min(3, len(events))] {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Content))
		}
	}

	if len(preferences) > 0 {
		sb.WriteString("\n### Preferences\n")
		for _, m := range preferences[:min(3, len(preferences))] {
			sb.WriteString(fmt.Sprintf("- %s\n", m.Content))
		}
	}

	return sb.String()
}

func mergeAndLimit(a, b []essentialMemory, limit int) []essentialMemory {
	merged := make([]essentialMemory, 0, limit)
	i, j := 0, 0
	for len(merged) < limit && (i < len(a) || j < len(b)) {
		if j >= len(b) || (i < len(a) && a[i].AccessCount >= b[j].AccessCount) {
			merged = append(merged, a[i])
			i++
		} else {
			merged = append(merged, b[j])
			j++
		}
	}
	return merged
}
