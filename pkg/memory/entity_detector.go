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
	"unicode/utf8"
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

// Loads past the first one read only the memories inserted since the last
// load, plus the keywords of the newest ones, so their cost tracks new writes
// instead of the corpus. Reading the newest rows again also covers a memory
// that reused a freed rowid at the watermark. Keywords edited or deleted on
// older memories stay in the lexicon until the process restarts; the lexicon
// only nudges ranking, so that drift is harmless.
const (
	entityFullLoadTimeout        = time.Minute
	entityIncrementalLoadTimeout = 5 * time.Second
)

// entityRecentLimit is how many of the newest memories every load reads again.
var entityRecentLimit = 500

type EntityDetector struct {
	db     *sql.DB
	cfg    EntityDetectorConfig
	logger *slog.Logger

	// loadSem admits one load at a time. Callers wait on the first load, up
	// to their context's end, so search sees the same lexicon on every run;
	// later refreshes run in the background, one at a time.
	loadSem chan struct{}

	// bgMu guards closed and the refreshes in bg, which run on bgCtx so
	// Close can stop them and wait before the store is closed.
	bgMu     sync.Mutex
	closed   bool
	bg       sync.WaitGroup
	bgCtx    context.Context
	bgCancel context.CancelFunc

	mu sync.RWMutex
	// keywords holds the keywords of every memory up to lastRowID.
	keywords map[string]struct{}
	// recent holds project names and the tokens of the newest memories,
	// rebuilt on every load.
	recent        map[string]struct{}
	lastRowID     int64
	loaded        bool
	lastAttemptAt time.Time
}

func NewEntityDetector(db *sql.DB, cfg EntityDetectorConfig, logger *slog.Logger) *EntityDetector {
	if logger == nil {
		logger = slog.Default()
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	return &EntityDetector{
		db:       db,
		cfg:      cfg.withDefaults(),
		logger:   logger,
		loadSem:  make(chan struct{}, 1),
		bgCtx:    bgCtx,
		bgCancel: bgCancel,
		keywords: make(map[string]struct{}),
		recent:   make(map[string]struct{}),
	}
}

// Detect returns the tokens of text that name a known entity. The first call
// in a process waits for the lexicon's first load, bounded by ctx.
func (d *EntityDetector) Detect(ctx context.Context, text string) []string {
	if err := d.ensureLexicon(ctx); err != nil {
		d.logger.Debug("entity detector: load failed", "err", err)
	}

	locs := entityTokenRe.FindAllStringIndex(text, -1)
	var entities []string
	seen := make(map[string]struct{})

	d.mu.RLock()
	defer d.mu.RUnlock()
	if len(d.keywords) == 0 && len(d.recent) == 0 {
		return nil
	}

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
		_, ok := d.keywords[normalized]
		if !ok {
			_, ok = d.recent[normalized]
		}
		if ok {
			seen[normalized] = struct{}{}
			entities = append(entities, raw)
		}
	}

	return entities
}

// Close stops background refreshes and waits for one in flight.
func (d *EntityDetector) Close() {
	d.bgMu.Lock()
	d.closed = true
	d.bgMu.Unlock()
	d.bgCancel()
	d.bg.Wait()
}

func (d *EntityDetector) Refresh(ctx context.Context) error {
	select {
	case d.loadSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-d.loadSem }()
	return d.loadLexicon(ctx)
}

func (d *EntityDetector) ensureLexicon(ctx context.Context) error {
	if d.sinceLastLoadAttempt() < d.cfg.CacheTTL {
		return nil
	}
	d.mu.RLock()
	loaded := d.loaded
	d.mu.RUnlock()
	if loaded {
		// Refresh off the search's path: this search answers from the
		// lexicon it has, and a refresh already running is not repeated.
		select {
		case d.loadSem <- struct{}{}:
		default:
			return nil
		}
		d.bgMu.Lock()
		if d.closed {
			d.bgMu.Unlock()
			<-d.loadSem
			return nil
		}
		d.bg.Add(1)
		d.bgMu.Unlock()
		go func() {
			defer d.bg.Done()
			defer func() { <-d.loadSem }()
			if d.sinceLastLoadAttempt() < d.cfg.CacheTTL {
				return
			}
			if err := d.loadLexicon(d.bgCtx); err != nil {
				d.logger.Debug("entity detector: refresh failed", "err", err)
			}
		}()
		return nil
	}

	select {
	case d.loadSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-d.loadSem }()

	if d.sinceLastLoadAttempt() < d.cfg.CacheTTL {
		return nil
	}
	return d.loadLexicon(ctx)
}

// loadLexicon reads the keywords of memories past lastRowID and rebuilds the
// recent set. Rows come in rowid order, so a load cut short keeps what it
// read and the next one resumes after it. A failed load still stamps
// lastAttemptAt: the next attempt waits a TTL instead of running on every search.
func (d *EntityDetector) loadLexicon(ctx context.Context) error {
	d.mu.RLock()
	after := d.lastRowID
	loaded := d.loaded
	d.mu.RUnlock()

	timeout := entityIncrementalLoadTimeout
	if !loaded {
		timeout = entityFullLoadTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var l lexiconLoad
	l.recent, l.recentKeywords, l.recentErr = d.loadRecent(tctx)
	l.keywords, l.lastRowID, l.keywordsErr = d.loadKeywords(tctx, after)
	// A load cut short because its caller gave up is not an attempt: the next
	// search tries again instead of waiting out the TTL with what it read.
	err := d.mergeLexicon(l, ctx.Err() == nil)
	if after == 0 && l.lastRowID > 0 {
		// The full read decoded every memory's keywords; hand those buffers
		// back instead of keeping them as heap headroom.
		releaseMemoryToOS()
	}
	return err
}

// lexiconLoad is what one load read, kept even when a query failed part way.
type lexiconLoad struct {
	recent         map[string]struct{}
	recentKeywords map[string]struct{}
	recentErr      error
	keywords       map[string]struct{}
	lastRowID      int64
	keywordsErr    error
}

// mergeLexicon folds one load into the lexicon and, when stamp is set, stamps
// the attempt, failed or not.
func (d *EntityDetector) mergeLexicon(l lexiconLoad, stamp bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.keywords) == 0 {
		d.keywords = l.keywords
	} else {
		for kw := range l.keywords {
			d.keywords[kw] = struct{}{}
		}
	}
	for kw := range l.recentKeywords {
		d.keywords[kw] = struct{}{}
	}
	if l.lastRowID > d.lastRowID {
		d.lastRowID = l.lastRowID
	}
	if l.recentErr == nil {
		d.recent = l.recent
	}
	if stamp {
		d.lastAttemptAt = time.Now()
	}

	if l.recentErr != nil {
		return l.recentErr
	}
	if l.keywordsErr != nil {
		return l.keywordsErr
	}
	d.loaded = true
	return nil
}

// loadRecent returns the tokens of project names and of the newest memories'
// content, and the keywords of those memories.
func (d *EntityDetector) loadRecent(ctx context.Context) (map[string]struct{}, map[string]struct{}, error) {
	recent := make(map[string]struct{})
	keywords := make(map[string]struct{})

	projRows, err := d.db.QueryContext(ctx, `SELECT name FROM projects`)
	if err != nil {
		return nil, nil, err
	}
	for projRows.Next() {
		var name string
		if err := projRows.Scan(&name); err != nil {
			continue
		}
		addEntityTokens(recent, name)
	}
	err = projRows.Err()
	projRows.Close()
	if err != nil {
		return nil, nil, err
	}

	// Newest by insertion: ordering by updated_at would scan and sort the
	// whole table on every refresh.
	contentRows, err := d.db.QueryContext(ctx,
		`SELECT content, keywords FROM memories ORDER BY rowid DESC LIMIT ?`, entityRecentLimit)
	if err != nil {
		return nil, nil, err
	}
	for contentRows.Next() {
		var content string
		var kwJSON sql.NullString
		if err := contentRows.Scan(&content, &kwJSON); err != nil {
			continue
		}
		addEntityTokens(recent, content)
		if kwJSON.Valid {
			addKeywords(keywords, []byte(kwJSON.String))
		}
	}
	err = contentRows.Err()
	contentRows.Close()
	if err != nil {
		return nil, nil, err
	}
	return recent, keywords, nil
}

// loadKeywords returns the keywords of memories with rowid > after and the
// last rowid it read, which is accurate even when the scan stops early.
func (d *EntityDetector) loadKeywords(ctx context.Context, after int64) (map[string]struct{}, int64, error) {
	keywords := make(map[string]struct{})
	lastRowID := after

	rows, err := d.db.QueryContext(ctx,
		`SELECT rowid, keywords FROM memories WHERE rowid > ? AND keywords IS NOT NULL ORDER BY rowid`, after)
	if err != nil {
		return keywords, lastRowID, err
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		// RawBytes skips copying each row's JSON; the decoded strings are
		// fresh allocations, so the map keys never alias it.
		var kwJSON sql.RawBytes
		if err := rows.Scan(&rowID, &kwJSON); err != nil {
			continue
		}
		lastRowID = rowID
		addKeywords(keywords, kwJSON)
	}
	return keywords, lastRowID, rows.Err()
}

// addKeywords adds the normalized entries of a memory's keywords JSON to set,
// skipping stop words; malformed JSON adds nothing.
func addKeywords(set map[string]struct{}, kwJSON []byte) {
	var kws []string
	if err := json.Unmarshal(kwJSON, &kws); err != nil {
		return
	}
	for _, kw := range kws {
		norm := normalizeEntity(kw)
		if norm != "" && !stopWords[norm] {
			set[norm] = struct{}{}
		}
	}
}

func (d *EntityDetector) sinceLastLoadAttempt() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.lastAttemptAt.IsZero() {
		return d.cfg.CacheTTL + 1
	}
	return time.Since(d.lastAttemptAt)
}

func normalizeEntity(s string) string {
	if isLowerASCII(s) {
		return s
	}
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

// isLowerASCII reports whether normalizeEntity would return s unchanged
// because it has no uppercase letters and no non-ASCII runes.
func isLowerASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= utf8.RuneSelf || ('A' <= c && c <= 'Z') {
			return false
		}
	}
	return true
}

func addEntityTokens(set map[string]struct{}, text string) {
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
		if _, ok := set[norm]; !ok {
			// norm may be a slice of text; a copy keeps the key from pinning
			// the whole memory content.
			set[strings.Clone(norm)] = struct{}{}
		}
	}
}
