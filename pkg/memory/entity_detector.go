package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// entityTokenRe matches Unicode word tokens >= 3 chars (allows hyphens, underscores).
var entityTokenRe = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_-]{2,}`)

type EntityDetectorConfig struct {
	CacheTTL    time.Duration
	MaxTokens   int
	MinTokenLen int
}

func DefaultEntityDetectorConfig() EntityDetectorConfig {
	return EntityDetectorConfig{
		CacheTTL:    30 * time.Second,
		MaxTokens:   40,
		MinTokenLen: 3,
	}
}

func (c EntityDetectorConfig) withDefaults() EntityDetectorConfig {
	if c.CacheTTL <= 0 {
		c.CacheTTL = 30 * time.Second
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 40
	}
	if c.MinTokenLen <= 0 {
		c.MinTokenLen = 3
	}
	return c
}

type EntityDetector struct {
	db     *sql.DB
	cfg    EntityDetectorConfig
	logger *slog.Logger

	mu         sync.RWMutex
	snapshot   map[string]struct{}
	loadedAt   time.Time
	refreshing bool
}

func NewEntityDetector(db *sql.DB, cfg EntityDetectorConfig, logger *slog.Logger) *EntityDetector {
	if logger == nil {
		logger = slog.Default()
	}
	return &EntityDetector{
		db:       db,
		cfg:      cfg.withDefaults(),
		logger:   logger,
		snapshot: make(map[string]struct{}),
	}
}

func (d *EntityDetector) Detect(text string) []string {
	if err := d.ensureSnapshot(context.Background()); err != nil {
		d.mu.RLock()
		empty := len(d.snapshot) == 0
		d.mu.RUnlock()
		if empty {
			return nil
		}
	}

	d.mu.RLock()
	snap := d.snapshot
	d.mu.RUnlock()

	if len(snap) == 0 {
		return nil
	}

	locs := entityTokenRe.FindAllStringIndex(text, -1)
	var entities []string
	seen := make(map[string]struct{})

	tokenCount := 0
	for _, loc := range locs {
		if tokenCount >= d.cfg.MaxTokens {
			break
		}
		raw := text[loc[0]:loc[1]]
		if len([]rune(raw)) < d.cfg.MinTokenLen {
			continue
		}
		tokenCount++

		normalized := normalizeEntity(raw)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		if _, ok := snap[normalized]; ok {
			seen[normalized] = struct{}{}
			entities = append(entities, raw)
		}
	}

	return entities
}

func (d *EntityDetector) Refresh(ctx context.Context) error {
	return d.loadSnapshot(ctx)
}

func (d *EntityDetector) ensureSnapshot(ctx context.Context) error {
	d.mu.RLock()
	stale := time.Since(d.loadedAt) >= d.cfg.CacheTTL
	refreshing := d.refreshing
	d.mu.RUnlock()

	if !stale {
		return nil
	}
	if refreshing {
		return nil
	}

	d.mu.Lock()
	if time.Since(d.loadedAt) < d.cfg.CacheTTL {
		d.mu.Unlock()
		return nil
	}
	if d.refreshing {
		d.mu.Unlock()
		return nil
	}
	d.refreshing = true
	d.mu.Unlock()

	err := d.loadSnapshot(ctx)

	d.mu.Lock()
	d.refreshing = false
	d.mu.Unlock()

	return err
}

func (d *EntityDetector) loadSnapshot(ctx context.Context) error {
	tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	newSnap := make(map[string]struct{})

	projRows, err := d.db.QueryContext(tctx, `SELECT name FROM projects`)
	if err != nil {
		d.logger.Debug("entity detector: projects query failed", "err", err)
		return err
	}
	for projRows.Next() {
		var name string
		if err := projRows.Scan(&name); err != nil {
			continue
		}
		addEntityTokens(newSnap, name)
	}
	projRows.Close()

	kwRows, err := d.db.QueryContext(tctx, `SELECT keywords FROM memories WHERE keywords IS NOT NULL`)
	if err != nil {
		d.logger.Debug("entity detector: keywords query failed", "err", err)
		return err
	}
	for kwRows.Next() {
		var kwJSON string
		if err := kwRows.Scan(&kwJSON); err != nil {
			continue
		}
		var kws []string
		if err := json.Unmarshal([]byte(kwJSON), &kws); err != nil {
			continue
		}
		for _, kw := range kws {
			norm := normalizeEntity(kw)
			if norm != "" && !stopWords[norm] {
				newSnap[norm] = struct{}{}
			}
		}
	}
	kwRows.Close()

	contentRows, err := d.db.QueryContext(tctx,
		`SELECT content FROM memories ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		d.logger.Debug("entity detector: content query failed", "err", err)
		return err
	}
	for contentRows.Next() {
		var content string
		if err := contentRows.Scan(&content); err != nil {
			continue
		}
		addEntityTokens(newSnap, content)
	}
	contentRows.Close()

	d.mu.Lock()
	d.snapshot = newSnap
	d.loadedAt = time.Now()
	d.mu.Unlock()

	return nil
}

func (d *EntityDetector) snapshotAge() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.loadedAt.IsZero() {
		return d.cfg.CacheTTL + 1
	}
	return time.Since(d.loadedAt)
}

func normalizeEntity(s string) string {
	s = strings.ToLower(s)
	var buf strings.Builder
	buf.Grow(len(s))
	for _, r := range s {
		if !unicode.Is(unicode.Mn, r) {
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func addEntityTokens(snap map[string]struct{}, text string) {
	locs := entityTokenRe.FindAllStringIndex(text, -1)
	for _, loc := range locs {
		raw := text[loc[0]:loc[1]]
		if len([]rune(raw)) < 3 {
			continue
		}
		norm := normalizeEntity(raw)
		if norm == "" {
			continue
		}
		if stopWords[norm] {
			continue
		}
		snap[norm] = struct{}{}
	}
}


