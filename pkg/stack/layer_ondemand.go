package stack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	defaultOnDemandBudget    = 1200
	defaultOnDemandMaxResults = 5
)

// OnDemandLayer retrieves contextual memories based on entities detected in
// the current query. Composes an EntityDetector with FTS5 search and optional
// topic change detection.
type OnDemandLayer struct {
	store          DBAccessor
	entityDetector EntityDetector
	topicDetector  TopicChangeChecker
	logger         *slog.Logger
	budget         int
	maxResults     int
}

// EntityDetector extracts known entities from text.
type EntityDetector interface {
	Detect(text string) []string
}

// TopicChangeChecker detects whether the topic has shifted between queries.
type TopicChangeChecker interface {
	Check(ctx context.Context, query string) (bool, error)
}

// OnDemandLayerConfig holds optional configuration.
type OnDemandLayerConfig struct {
	Budget     int
	MaxResults int
}

func (c OnDemandLayerConfig) withDefaults() OnDemandLayerConfig {
	if c.Budget <= 0 {
		c.Budget = defaultOnDemandBudget
	}
	if c.MaxResults <= 0 {
		c.MaxResults = defaultOnDemandMaxResults
	}
	return c
}

// NewOnDemandLayer creates a real on-demand retrieval layer.
// store provides DB access for FTS5 queries.
// entityDetector extracts entities from query text.
// topicDetector may be nil — topic change detection is optional.
func NewOnDemandLayer(store DBAccessor, entityDetector EntityDetector, topicDetector TopicChangeChecker, logger *slog.Logger, cfg OnDemandLayerConfig) *OnDemandLayer {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	return &OnDemandLayer{
		store:          store,
		entityDetector: entityDetector,
		topicDetector:  topicDetector,
		logger:         logger,
		budget:         cfg.Budget,
		maxResults:     cfg.MaxResults,
	}
}

// Render retrieves memories relevant to the query entities and returns a
// formatted markdown snippet. If query is empty, returns "".
func (l *OnDemandLayer) Render(query, projectID string) string {
	if l == nil || query == "" {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()

	entities := l.entityDetector.Detect(query)
	if len(entities) == 0 {
		return ""
	}

	ftsQuery := l.buildFTSQuery(entities)
	if ftsQuery == "" {
		return ""
	}

	topicChanged := false
	if l.topicDetector != nil {
		changed, err := l.topicDetector.Check(ctx, query)
		if err != nil {
			l.logger.Debug("on-demand: topic check failed", "err", err)
		} else {
			topicChanged = changed
		}
	}

	limit := l.maxResults
	if topicChanged {
		limit = l.maxResults + 3
	}

	results := l.searchFTS5(ctx, ftsQuery, projectID, limit)
	if len(results) == 0 {
		return ""
	}

	if topicChanged {
		results = diversifyCategories(results, l.maxResults)
	}

	output := l.renderMarkdown(results)
	output = truncateAtBoundary(output, l.budget)

	return output
}

// onDemandMemory is a single retrieved memory.
type onDemandMemory struct {
	Category string
	Content  string
}

// buildFTSQuery joins entities into a valid FTS5 MATCH expression.
// Each entity is quoted and joined with OR: "entity1" OR "entity2"
func (l *OnDemandLayer) buildFTSQuery(entities []string) string {
	if len(entities) == 0 {
		return ""
	}
	var parts []string
	for _, e := range entities {
		clean := sanitizeFTSEntity(e)
		if clean != "" {
			parts = append(parts, fmt.Sprintf(`"%s"`, clean))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}

// sanitizeFTSEntity removes FTS5 special characters from an entity string.
func sanitizeFTSEntity(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '(', ')', '*', '^', ':', '{', '}':
			return -1
		default:
			return r
		}
	}, s)
}

// searchFTS5 queries the FTS5 index with the given match expression,
// optionally filtered by projectID.
func (l *OnDemandLayer) searchFTS5(ctx context.Context, ftsQuery, projectID string, limit int) []onDemandMemory {
	var query string
	var args []any

	if projectID != "" {
		query = `SELECT m.category, m.content
			FROM memories_fts fts
			JOIN memories m ON m.rowid = fts.rowid
			WHERE memories_fts MATCH ? AND m.project_id = ?
			ORDER BY bm25(memories_fts)
			LIMIT ?`
		args = []any{ftsQuery, projectID, limit}
	} else {
		query = `SELECT m.category, m.content
			FROM memories_fts fts
			JOIN memories m ON m.rowid = fts.rowid
			WHERE memories_fts MATCH ?
			ORDER BY bm25(memories_fts)
			LIMIT ?`
		args = []any{ftsQuery, limit}
	}

	rows, err := l.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		l.logger.Debug("on-demand: FTS5 query failed", "err", err, "query", ftsQuery)
		return nil
	}
	defer rows.Close()

	var results []onDemandMemory
	for rows.Next() {
		var m onDemandMemory
		if err := rows.Scan(&m.Category, &m.Content); err != nil {
			continue
		}
		results = append(results, m)
	}
	return results
}

// diversifyCategories picks results from as many distinct categories as
// possible, up to maxResults total.
func diversifyCategories(results []onDemandMemory, maxResults int) []onDemandMemory {
	seen := make(map[string]bool)
	var diversified []onDemandMemory

	// First pass: one from each category.
	for _, r := range results {
		if !seen[r.Category] {
			seen[r.Category] = true
			diversified = append(diversified, r)
		}
		if len(diversified) >= maxResults {
			return diversified
		}
	}

	// Second pass: fill remaining.
	for _, r := range results {
		if len(diversified) >= maxResults {
			break
		}
		// Already included in first pass.
		var alreadyIncluded bool
		for _, d := range diversified {
			if d.Category == r.Category && d.Content == r.Content {
				alreadyIncluded = true
				break
			}
		}
		if !alreadyIncluded {
			diversified = append(diversified, r)
		}
	}

	return diversified
}

// renderMarkdown formats memories as a markdown bullet list.
func (l *OnDemandLayer) renderMarkdown(results []onDemandMemory) string {
	var b strings.Builder
	b.WriteString("### Related Memories\n")

	for _, r := range results {
		snippet := r.Content
		// Truncate long content to ~120 chars for readability.
		if len(snippet) > 120 {
			snippet = snippet[:120]
			if idx := strings.LastIndex(snippet, " "); idx > 80 {
				snippet = snippet[:idx]
			}
			snippet += "..."
		}
		b.WriteString(fmt.Sprintf("- **[%s]** %s\n", r.Category, snippet))
	}

	return b.String()
}

// truncateAtBoundary truncates output to at most maxBytes, preferring to
// truncate at the last newline boundary.
func truncateAtBoundary(output string, maxBytes int) string {
	if len(output) <= maxBytes {
		return output
	}
	output = output[:maxBytes]
	if i := strings.LastIndex(output, "\n"); i > 0 {
		return output[:i]
	}
	return output
}
