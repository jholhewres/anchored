package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/hookroute"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/session"

	_ "github.com/mattn/go-sqlite3"
)

// stopHardCap is the total wall-clock budget for the Stop hook.
const stopHardCap = 500 * time.Millisecond

// stopMaxSaves is the maximum number of memories saved per invocation.
const stopMaxSaves = 2

// stopDedupCandidates is how many recent project memories we compare against.
const stopDedupCandidates = 50

// stopJaccardThreshold is the Jaccard similarity above which a candidate is
// considered a duplicate of an existing memory.
const stopJaccardThreshold = 0.7

// stopMinRunes is the minimum rune length of a durable candidate (drops trivial).
const stopMinRunes = 80

// stopMaxRunes is the maximum rune length of a durable candidate.
const stopMaxRunes = 700

// stopTTLDays is the default TTL for auto-extracted memories. They promote to
// permanent when referenced (future Feature D/E hook).
const stopTTLDays = 30

// durableCandidate is an extracted memory candidate from the transcript tail.
type durableCandidate struct {
	Text     string
	Category string // "decision" | "learning" | "event"
}

// decisionPatterns are regex patterns (PT+EN) that signal a durable decision/learning.
var decisionPatterns = []*regexp.Regexp{
	// PT
	regexp.MustCompile(`(?i)decidi(?:do|mos|r)?`),
	regexp.MustCompile(`(?i)causa\s+raiz`),
	regexp.MustCompile(`a\s+solu[çc][aã]o\s+foi`),
	regexp.MustCompile(`(?i)vamos\s+usar`),
	regexp.MustCompile(`(?i)li[çc][aã]o`),
	// EN
	regexp.MustCompile(`(?i)root\s+cause`),
	regexp.MustCompile(`(?i)fixed\s+by`),
	regexp.MustCompile(`(?i)lesson`),
	regexp.MustCompile(`(?i)settled\s+on`),
	regexp.MustCompile(`(?i)released\s+v`),
	regexp.MustCompile(`(?i)deployed`),
}

// categoryForPattern maps pattern index → category.
// decision: decidi, root cause, settled on, released, deployed, vamos usar
// learning: causa raiz, a solução foi, fixed by, lesson, lição
var patternCategory = []string{
	"decision", // decidi
	"learning", // causa raiz
	"learning", // a solução foi
	"decision", // vamos usar
	"learning", // lição
	"learning", // root cause
	"learning", // fixed by
	"learning", // lesson
	"decision", // settled on
	"event",    // released v
	"event",    // deployed
}

// runHookStop implements the Claude Code Stop hook.
// Input JSON: {session_id, transcript_path, stop_hook_active, cwd}
// Always exits 0 (fail-safe).
func runHookStop(args []string) {
	fs := newFlagSet("hook stop")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	// Hard deadline for the entire hook.
	deadline := time.Now().Add(stopHardCap)

	dlog := openDebugLogger(*configPath)
	defer dlog.Close()

	body, _ := io.ReadAll(os.Stdin)

	// Cursor's stop payload carries no transcript_path (a Claude Code concept),
	// so the durable-extraction pass below has nothing to read. Route it through
	// the Cursor adapter, which exits 0 fast. Claude Code payloads never match.
	if hookroute.DetectCursorPayload(body) {
		handleCursorHook(os.Stdout, body, *configPath, dlog)
		return
	}

	var input struct {
		SessionID      string `json:"session_id"`
		TranscriptPath string `json:"transcript_path"`
		StopHookActive bool   `json:"stop_hook_active"`
		Cwd            string `json:"cwd"`
	}
	_ = json.Unmarshal(body, &input)

	// Guard: if stop_hook_active=true we are called recursively — exit immediately.
	if input.StopHookActive {
		dlog.Event("hook.stop", map[string]any{"stage": "loop_guard"})
		outputJSON(map[string]any{"saved": 0})
		return
	}

	cfg, _ := loadConfig(*configPath)
	if cfg != nil && !cfg.Plugin.AutoSaveStopEnabled() {
		dlog.Event("hook.stop", map[string]any{"stage": "disabled"})
		outputJSON(map[string]any{"saved": 0})
		return
	}

	// Read transcript tail (last ~64KB).
	transcriptText := readTranscriptTail(input.TranscriptPath, 64*1024)
	if transcriptText == "" {
		outputJSON(map[string]any{"saved": 0})
		return
	}

	// Extract durable candidates. The used-signal pass below needs the write
	// context even when there is nothing to save, so candidates==0 no longer
	// short-circuits before the DB is open.
	candidates := extractDurableCandidates(transcriptText)

	// Open write context (no embedder, busy_timeout ≤ 300ms).
	hc, err := openHookContextWrite(*configPath)
	if err != nil {
		dlog.Event("hook.stop", map[string]any{"stage": "db_open_failed", "error": err.Error()})
		outputJSON(map[string]any{"saved": 0})
		return
	}
	defer hc.Close()

	cwdVal := input.Cwd
	if cwdVal == "" {
		cwdVal = "."
	}
	projectID := hc.ResolveProject(cwdVal)

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	// Usage-feedback (Feature D): mark which injected memories the turn's text
	// actually drew on, closing the loop that recordInjection opens. Runs
	// before the save pass so it still happens when there are no candidates.
	used := markUsedMemories(ctx, hc, input.SessionID, transcriptText, dlog)

	if len(candidates) == 0 {
		dlog.Event("hook.stop", map[string]any{"stage": "no_candidates", "used": used})
		outputJSON(map[string]any{"saved": 0, "used": used})
		return
	}

	// Load recent memories for dedup. If the set can't be loaded completely,
	// skip saving this turn — saving without dedup risks duplicates, and the
	// next turn's Stop hook will recapture the same candidates.
	recentContents, err := loadRecentMemoryContents(ctx, hc, projectID, stopDedupCandidates)
	if err != nil {
		dlog.Event("hook.stop", map[string]any{"stage": "dedup_load_failed", "error": err.Error()})
		outputJSON(map[string]any{"saved": 0})
		return
	}

	saved := 0
	for _, c := range candidates {
		if saved >= stopMaxSaves {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		if isDuplicate(c.Text, recentContents) {
			dlog.Event("hook.stop", map[string]any{"stage": "dedup_skip", "head": debuglog.Snippet(c.Text, 80)})
			continue
		}
		id := newHookID()
		if err := saveMemoryLightweight(ctx, hc, id, projectID, c.Category, c.Text, input.SessionID); err != nil {
			dlog.Event("hook.stop", map[string]any{"stage": "save_failed", "error": err.Error()})
			continue
		}
		dlog.Event("hook.stop", map[string]any{
			"stage":    "saved",
			"id":       id,
			"category": c.Category,
			"head":     debuglog.Snippet(c.Text, 80),
		})
		// Add to in-memory set so the next candidate deduplicates against it too.
		recentContents = append(recentContents, c.Text)
		saved++
	}

	dlog.Event("hook.stop", map[string]any{"stage": "done", "saved": saved, "candidates": len(candidates), "used": used})
	outputJSON(map[string]any{"saved": saved, "used": used})
}

// usedMinOverlap / usedMinRatio define when a memory counts as "used" by the
// turn: at least 3 of its significant tokens appear in the turn text, or at
// least 2 covering 30% of a short memory's significant tokens. Deterministic
// on purpose — no model in the loop.
const (
	usedMinOverlap    = 3
	usedMinRatio      = 0.30
	usedMinTokenRunes = 5
)

// markUsedMemories closes the usage-feedback loop: for every memory the
// session injected (working_sets.memory_ids, fed by the UserPromptSubmit
// hook), check whether the turn's transcript text draws on its content and
// bump used_count/last_used_at. Best-effort and bounded by the caller's
// deadline; returns how many memories were marked.
func markUsedMemories(ctx context.Context, hc *HookContext, sessionID, turnText string, dlog *debuglog.Logger) int {
	if sessionID == "" || turnText == "" {
		return 0
	}
	mgr := session.NewManager(hc.db, nil)
	ws, err := mgr.GetWorkingSet(ctx, sessionID)
	if err != nil || ws == nil || len(ws.MemoryIDs) == 0 {
		return 0
	}

	placeholders := make([]string, len(ws.MemoryIDs))
	args := make([]any, len(ws.MemoryIDs))
	for i, id := range ws.MemoryIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := hc.db.QueryContext(ctx, `
		SELECT id, content FROM memories
		WHERE deleted_at IS NULL AND id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()

	turnTokens := significantTokenSet(turnText)
	var usedIDs []string
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			continue
		}
		memTokens := significantTokenSet(content)
		if len(memTokens) == 0 {
			continue
		}
		overlap := 0
		for tok := range memTokens {
			if turnTokens[tok] {
				overlap++
			}
		}
		// The absolute floor scales with the memory's vocabulary: 3 shared
		// tokens means a lot against a 8-token memory and nothing against a
		// 40-token one (generic dev terms would false-positive). Long
		// memories effectively require usedMinRatio coverage.
		minNeeded := usedMinOverlap
		if len(memTokens) > 15 {
			if scaled := int(float64(len(memTokens)) * usedMinRatio); scaled > minNeeded {
				minNeeded = scaled
			}
		}
		ratio := float64(overlap) / float64(len(memTokens))
		if overlap >= minNeeded || (overlap >= 2 && ratio >= usedMinRatio) {
			usedIDs = append(usedIDs, id)
		}
	}
	if err := rows.Err(); err != nil || len(usedIDs) == 0 {
		return 0
	}

	upPlaceholders := make([]string, len(usedIDs))
	upArgs := make([]any, 0, len(usedIDs)+1)
	upArgs = append(upArgs, time.Now().UTC().Format(time.RFC3339))
	for i, id := range usedIDs {
		upPlaceholders[i] = "?"
		upArgs = append(upArgs, id)
	}
	// Metadata-only UPDATE: does not fire the FTS triggers (AFTER UPDATE OF
	// content, keywords). The NULLIF chain normalises legacy ''/'null' rows.
	if _, err := hc.db.ExecContext(ctx, `
		UPDATE memories SET metadata = json_set(
			COALESCE(NULLIF(NULLIF(metadata, ''), 'null'), '{}'),
			'$.used_count', COALESCE(json_extract(metadata, '$.used_count'), 0) + 1,
			'$.last_used_at', ?
		) WHERE id IN (`+strings.Join(upPlaceholders, ",")+`)`, upArgs...); err != nil {
		dlog.Event("hook.stop.used", map[string]any{"stage": "update_failed", "error": err.Error()})
		return 0
	}
	dlog.Event("hook.stop.used", map[string]any{"stage": "marked", "count": len(usedIDs)})
	return len(usedIDs)
}

// significantTokenSet lower-cases and splits text on non-alphanumeric runes,
// keeping tokens of usedMinTokenRunes+ runes. Splitting on punctuation (not
// just whitespace) lets code identifiers inside backticks/parens match.
func significantTokenSet(text string) map[string]bool {
	out := make(map[string]bool)
	var cur []rune
	flush := func() {
		if len(cur) >= usedMinTokenRunes {
			out[string(cur)] = true
		}
		cur = cur[:0]
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// openHookContextWrite opens the SQLite DB in write mode with busy_timeout ≤ 300ms.
// No embedder is loaded; the curation worker handles embedding after the fact.
func openHookContextWrite(configPath string) (*HookContext, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	// On a fresh install the data dir may not exist yet; without this the
	// lazy sql.Open succeeds but the first write fails forever (silently,
	// given the fail-safe), so the stop hook would never work.
	if err := config.EnsureDirs(cfg); err != nil {
		return nil, fmt.Errorf("ensure dirs: %w", err)
	}

	// Limit busy_timeout to 300ms so a locked DB doesn't blow the hard cap.
	dsn := cfg.Memory.DatabasePath + "?_journal_mode=WAL&_busy_timeout=300"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	return &HookContext{
		cfg:      cfg,
		db:       db,
		detector: project.NewDetector(db),
	}, nil
}

// saveMemoryLightweight performs a raw INSERT into memories with embedding=NULL.
// The curation worker will compute the embedding asynchronously.
func saveMemoryLightweight(ctx context.Context, hc *HookContext, id, projectID, category, content, sessionID string) error {
	hash := contentHashStop(content)
	now := time.Now().UTC()
	expiresAt := now.Add(stopTTLDays * 24 * time.Hour).Format(time.RFC3339)

	scope := memory.ScopeProject
	if projectID == "" {
		scope = memory.ScopeUser
	}

	meta := memory.MemoryMetadata{
		MemoryType: memory.MemoryTypeSemantic,
		Kind:       category,
		Origin:     memory.OriginHook,
		Scope:      scope,
		ExpiresAt:  expiresAt,
		Source:     "stop_hook",
		Extra:      map[string]any{"kind": "auto_extracted", "session_id": sessionID},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		metaJSON = []byte("{}")
	}

	var projID *string
	if projectID != "" {
		projID = &projectID
	}

	_, err = hc.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO memories
		 (id, project_id, category, content, content_hash, keywords, embedding,
		  source, created_at, updated_at, access_count, metadata, sync_dirty)
		 VALUES (?, ?, ?, ?, ?, '[]', NULL, 'stop_hook', ?, ?, 0, ?, 0)`,
		id, projID, category, content, hash,
		now, now, string(metaJSON),
	)
	return err
}

// contentHashStop computes SHA-256 of content (same as sqlite_store.go).
func contentHashStop(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// extractDurableCandidates scans text for sentences/paragraphs containing
// decision/learning markers (PT+EN). Each candidate is capped at stopMaxRunes
// and must be at least stopMinRunes runes.
func extractDurableCandidates(text string) []durableCandidate {
	// Split on paragraph breaks first, then on sentence boundaries.
	paragraphs := splitParagraphs(text)
	var out []durableCandidate
	seen := make(map[string]bool)

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		// Try to find a sentence within the paragraph that contains a marker.
		// A marker inside a sentence shorter than stopMinRunes falls back to
		// the whole paragraph (capped) so short decisive sentences — common in
		// PT — are not silently dropped.
		sentences := splitSentences(para)
		matchedSentence := false
		shortMatchCat := ""
		for _, sent := range sentences {
			sent = strings.TrimSpace(sent)
			cat, matched := matchDecisionPattern(sent)
			if !matched {
				continue
			}
			if utf8.RuneCountInString(sent) < stopMinRunes {
				if shortMatchCat == "" {
					shortMatchCat = cat
				}
				continue
			}
			matchedSentence = true
			capped := stopTruncateRunes(sent, stopMaxRunes)
			key := contentHashStop(strings.ToLower(capped))
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, durableCandidate{Text: capped, Category: cat})
		}
		if !matchedSentence && shortMatchCat != "" &&
			utf8.RuneCountInString(para) >= stopMinRunes {
			capped := stopTruncateRunes(para, stopMaxRunes)
			key := contentHashStop(strings.ToLower(capped))
			if !seen[key] {
				seen[key] = true
				out = append(out, durableCandidate{Text: capped, Category: shortMatchCat})
			}
		}
	}
	return out
}

// matchDecisionPattern returns the category and true if text matches any marker.
func matchDecisionPattern(text string) (string, bool) {
	for i, re := range decisionPatterns {
		if re.MatchString(text) {
			cat := "decision"
			if i < len(patternCategory) {
				cat = patternCategory[i]
			}
			return cat, true
		}
	}
	return "", false
}

// splitParagraphs splits text on blank lines.
func splitParagraphs(text string) []string {
	return strings.Split(text, "\n\n")
}

// splitSentences splits text on sentence boundaries (. ! ?) and newlines.
func splitSentences(text string) []string {
	// Rough split: treat '. ', '! ', '? ', '\n' as boundaries.
	var parts []string
	var cur strings.Builder
	runes := []rune(text)
	n := len(runes)
	for i := 0; i < n; i++ {
		r := runes[i]
		cur.WriteRune(r)
		if r == '\n' || ((r == '.' || r == '!' || r == '?') && i+1 < n && runes[i+1] == ' ') {
			s := strings.TrimSpace(cur.String())
			if s != "" {
				parts = append(parts, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		parts = append(parts, s)
	}
	return parts
}

// stopTruncateRunes caps s at max runes.
func stopTruncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}

// loadRecentMemoryContents returns the content of the N most recent memories
// for the project. Used for Jaccard dedup. A non-nil error means the dedup set
// could not be loaded completely — the caller must skip saving this turn
// rather than risk inserting duplicates of the memories it couldn't see.
func loadRecentMemoryContents(ctx context.Context, hc *HookContext, projectID string, n int) ([]string, error) {
	rows, err := hc.db.QueryContext(ctx,
		`SELECT content FROM memories
		 WHERE deleted_at IS NULL
		   AND (project_id = ? OR project_id IS NULL OR project_id = '')
		 ORDER BY created_at DESC LIMIT ?`,
		projectID, n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err == nil {
			out = append(out, content)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isDuplicate returns true when candidate is a near-duplicate of any existing
// content (exact content-hash match OR Jaccard ≥ 0.7).
func isDuplicate(candidate string, existing []string) bool {
	hash := contentHashStop(candidate)
	candTokens := stopTokenize(candidate)

	for _, ex := range existing {
		if contentHashStop(ex) == hash {
			return true
		}
		if stopJaccard(candTokens, stopTokenize(ex)) >= stopJaccardThreshold {
			return true
		}
	}
	return false
}

// stopTokenize uses strings.Fields(strings.ToLower(text)) — the same
// tokenisation as jaccardSimilarity in pkg/memory/hybrid_search.go.
func stopTokenize(text string) map[string]bool {
	out := make(map[string]bool)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		out[w] = true
	}
	return out
}

// stopJaccard computes the Jaccard similarity between two token sets.
func stopJaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	intersection := 0
	for tok := range a {
		if b[tok] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// readTranscriptTail reads up to maxBytes from the tail of a JSONL transcript
// file and returns the concatenated assistant/user text content.
// Returns "" on any read/parse error (fail-safe).
func readTranscriptTail(path string, maxBytes int) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Seek to the last maxBytes of the file.
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	if size > int64(maxBytes) {
		if _, err := f.Seek(-int64(maxBytes), io.SeekEnd); err != nil {
			return ""
		}
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}

	// Parse JSONL lines tolerantly; extract text from assistant/user messages.
	var sb strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content any    `json:"content"`
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // tolerant: skip unparseable lines
		}

		// Support both top-level role and nested message.role.
		role := msg.Role
		content := msg.Content
		if role == "" && msg.Message.Role != "" {
			role = msg.Message.Role
			content = msg.Message.Content
		}

		if role != "assistant" && role != "user" {
			continue
		}

		text := extractTextContent(content)
		if text != "" {
			sb.WriteString(text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// extractTextContent extracts plain text from a Claude message content field,
// which can be a string or []{"type":"text","text":"..."} array.
func extractTextContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	// Try JSON array of content blocks.
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, " ")
}
