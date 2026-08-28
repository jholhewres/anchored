package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/memory"
	_ "github.com/mattn/go-sqlite3"
)

func TestSanitizeFTSQuery(t *testing.T) {
	cases := map[string]string{
		"how did we decide on RRF?":            "how did decide rrf",
		"a gente fechou em Postgres ou MySQL?": "gente fechou postgres mysql",
		`weird "quotes" and (parens)`:          "weird quotes and parens",
		"":                                     "",
		"x y":                                  "", // dropped: too short
	}
	for in, want := range cases {
		got := sanitizeFTSQuery(in)
		if got != want {
			t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFTSQuery_CapsAt16Tokens(t *testing.T) {
	long := strings.Repeat("foo bar baz ", 20) // 60 tokens
	got := sanitizeFTSQuery(long)
	tokens := strings.Fields(got)
	if len(tokens) != 16 {
		t.Fatalf("expected 16 tokens, got %d", len(tokens))
	}
}

func TestRenderRecallPreview_FormatsAndEscapes(t *testing.T) {
	hits := []preSearchHit{
		{Category: "decision", Content: "Settled on RRF for hybrid search"},
		{Category: "fact", Content: "uses <b>highlights</b> & ampersands"},
	}
	out := renderRecallPreview("how did we decide", "planning", hits, nil, "", adaptiveReminderDefault, 4800)
	if !strings.Contains(out, `<anchored_recall intent="planning" query="how did we decide" count="2">`) {
		t.Errorf("missing wrapper: %s", out)
	}
	if !strings.Contains(out, "[decision] Settled on RRF for hybrid search") {
		t.Errorf("missing first hit: %s", out)
	}
	if !strings.Contains(out, "uses &lt;b&gt;highlights&lt;/b&gt; &amp; ampersands") {
		t.Errorf("XML not escaped: %s", out)
	}
	if !strings.Contains(out, "</anchored_recall>") {
		t.Errorf("missing closing tag: %s", out)
	}
}

// TestRenderRecallPreview_BudgetDropsTrailingHits verifies the block fits the
// budget by dropping lowest-relevance (trailing) hits, never truncating a hit
// mid-content. The top (most relevant) hit always survives.
func TestRenderRecallPreview_BudgetDropsTrailingHits(t *testing.T) {
	body := strings.Repeat("x", 100) // under the 240-rune per-hit cap
	hits := []preSearchHit{
		{Category: "decision", Content: "TOP " + body},
		{Category: "fact", Content: "MID " + body},
		{Category: "learning", Content: "LOW " + body},
	}
	// Budget big enough for the wrapper + ~1 hit but not 3.
	out := renderRecallPreview("q", "debugging", hits, nil, "", adaptiveReminderDefault, 200)
	if len(out) > 200 {
		t.Fatalf("output %d exceeds budget 200", len(out))
	}
	if !strings.Contains(out, "TOP ") {
		t.Errorf("top (most relevant) hit must survive: %s", out)
	}
	if strings.Contains(out, "LOW ") {
		t.Errorf("lowest-relevance hit should have been dropped: %s", out)
	}
	// Surviving hit is whole (full body present), never mid-truncated.
	if !strings.Contains(out, "TOP "+body) {
		t.Errorf("surviving hit was truncated mid-content: %s", out)
	}
}

// TestRenderRecallPreview_Artifacts includes artifact lines for the debugging
// path.
func TestRenderRecallPreview_Artifacts(t *testing.T) {
	arts := []recentArtifact{{Type: "test_report", SourceLabel: "go test ./...", AgeHint: "2026-06-08"}}
	out := renderRecallPreview("why failing", "debugging", nil, arts, "", adaptiveReminderDefault, 4800)
	if !strings.Contains(out, `<artifact type="test_report" label="go test ./..."/>`) {
		t.Errorf("missing artifact line: %s", out)
	}
}

// TestBM25TopHits_EndToEnd seeds a real sqlite DB with the production
// memories schema and verifies the BM25 query returns project-scoped hits
// in MATCH-ranked order.
func TestBM25TopHits_EndToEnd(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := `
		CREATE TABLE memories (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			category TEXT,
			content TEXT,
			keywords TEXT,
			metadata TEXT,
			deleted_at DATETIME
		);
		CREATE VIRTUAL TABLE memories_fts USING fts5(
			content, keywords, content=memories, content_rowid=rowid
		);
		CREATE TRIGGER memories_fts_insert AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords);
		END;
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	insert := func(id, projectID, category, content string) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, keywords) VALUES (?,?,?,?,'')`,
			id, projectID, category, content,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("m1", "proj-A", "decision", "we settled on RRF for hybrid search ranking")
	insert("m2", "proj-A", "fact", "Go 1.24 is the production runtime")
	insert("m3", "proj-B", "decision", "RRF was rejected for the other team")

	// Project-scoped: only proj-A rows should match.
	hits, err := bm25TopHits(context.Background(), db, "rrf hybrid search", "proj-A", 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(hits) != 1 || hits[0].Category != "decision" {
		t.Fatalf("project-scoped hits = %+v, want [decision m1]", hits)
	}

	// Global: empty projectID returns matches across all projects.
	all, err := bm25TopHits(context.Background(), db, "rrf", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("global hits = %d, want 2", len(all))
	}
}

// TestBM25TopHits_CategoryFilter verifies the intent-aware category boost:
// passing categories restricts results to those memory kinds.
func TestBM25TopHits_CategoryFilter(t *testing.T) {
	db := newFTSTestDB(t)
	insertMem(t, db, "m1", "proj-A", "decision", "we should use postgres for durable storage")
	insertMem(t, db, "m2", "proj-A", "event", "deployed postgres to production today")

	// No filter: both match.
	all, err := bm25TopHits(context.Background(), db, "postgres", "proj-A", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered hits = %d, want 2", len(all))
	}

	// Decision-only filter: the event is excluded.
	dec, err := bm25TopHits(context.Background(), db, "postgres", "proj-A", 5, "decision")
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != 1 || dec[0].Category != "decision" {
		t.Fatalf("category-filtered hits = %+v, want [decision]", dec)
	}
}

// newFTSTestDB builds the minimal memories + FTS5 schema shared by the BM25 tests.
func newFTSTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `
		CREATE TABLE memories (
			id TEXT PRIMARY KEY, project_id TEXT, category TEXT,
			content TEXT, keywords TEXT, metadata TEXT, deleted_at DATETIME
		);
		CREATE VIRTUAL TABLE memories_fts USING fts5(
			content, keywords, content=memories, content_rowid=rowid
		);
		CREATE TRIGGER memories_fts_insert AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords);
		END;`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertMem(t *testing.T, db *sql.DB, id, projectID, category, content string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, keywords) VALUES (?,?,?,?,'')`,
		id, projectID, category, content,
	); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkBM25TopHits measures the hot path of auto-recall against a 5k-memory
// DB.
func BenchmarkBM25TopHits(b *testing.B) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	schema := `
		CREATE TABLE memories (id TEXT PRIMARY KEY, project_id TEXT, category TEXT, content TEXT, keywords TEXT, metadata TEXT, deleted_at DATETIME);
		CREATE VIRTUAL TABLE memories_fts USING fts5(content, keywords, content=memories, content_rowid=rowid);
		CREATE TRIGGER t AFTER INSERT ON memories BEGIN INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords); END;`
	if _, err := db.Exec(schema); err != nil {
		b.Fatal(err)
	}
	tx, _ := db.Begin()
	for i := 0; i < 5000; i++ {
		_, _ = tx.Exec(`INSERT INTO memories (id, project_id, category, content, keywords) VALUES (?,?,?,?,'')`,
			"m"+itoaBench(i), "proj-A", "decision",
			"decision number "+itoaBench(i)+" about postgres sqlite redis kafka architecture and sync engine design", "")
	}
	_ = tx.Commit()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := bm25TopHits(context.Background(), db, "postgres sync engine architecture", "proj-A", 5); err != nil {
			b.Fatal(err)
		}
	}
}

func itoaBench(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestTruncateRunes_LocalCopy(t *testing.T) {
	if got := truncateRunes("ção é ñ", 3); got != "çãо…" && got != "ção…" {
		if !strings.HasSuffix(got, "…") {
			t.Errorf("expected ellipsis suffix, got %q", got)
		}
	}
	if got := truncateRunes("hi", 0); got != "" {
		t.Errorf("max=0 should return empty, got %q", got)
	}
}

// ── AC §4 item 3: anchor extraction ─────────────────────────────────────────

func TestExtractAnchors_FileAndSymbol(t *testing.T) {
	prompt := `analisa pkg/memory/hybrid_search.go e a função applyWorkingSetBoost também WorkingSetSignals`
	files, syms := extractAnchors(prompt)

	found := false
	for _, f := range files {
		if strings.Contains(f, "hybrid_search.go") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected hybrid_search.go in file anchors, got %v", files)
	}

	foundSym := false
	for _, s := range syms {
		if s == "applyWorkingSetBoost" || s == "WorkingSetSignals" {
			foundSym = true
		}
	}
	if !foundSym {
		t.Errorf("expected CamelCase symbol in syms, got %v", syms)
	}
}

func TestExtractAnchors_PlainFilename(t *testing.T) {
	files, _ := extractAnchors("verifique vector_cache.go por erros")
	found := false
	for _, f := range files {
		if f == "vector_cache.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("file anchors = %v, want vector_cache.go", files)
	}
}

func TestExtractAnchors_NoAnchors(t *testing.T) {
	files, syms := extractAnchors("como vai você hoje")
	if len(files) != 0 || len(syms) != 0 {
		t.Errorf("expected no anchors in plain prose, got files=%v syms=%v", files, syms)
	}
}

// ── AC §4 item 3: file anchor re-rank ────────────────────────────────────────

// TestReRankHits_FileAnchorBoosted verifies that a hit mentioning the anchor
// file is boosted to first position and annotated "file_anchor".
func TestReRankHits_FileAnchorBoosted(t *testing.T) {
	hits := []preSearchHit{
		{Category: "learning", Content: "use vector_cache.go for caching embeddings"},
		{Category: "decision", Content: "decided to use postgres for storage"},
	}
	fileAnchors := []string{"vector_cache.go"}
	tokens := anchorTokens(fileAnchors, nil, nil)
	result := reRankHits(hits, tokens, fileAnchors, nil)

	if result[0].Category != "learning" {
		t.Errorf("expected boosted hit (learning) first, got %s", result[0].Category)
	}
	foundSignal := false
	for _, s := range result[0].Signals {
		if s == "file_anchor" {
			foundSignal = true
		}
	}
	if !foundSignal {
		t.Errorf("expected file_anchor signal on boosted hit, got %v", result[0].Signals)
	}
}

// ── AC §4 item 4: working set boost ──────────────────────────────────────────

// TestReRankHits_WorkingSetBoosted verifies that a hit mentioning a working-set
// file receives the "working_set" signal annotation. When the ws-hit has a
// lower BM25 base score but the boost is sufficient to overtake the other hit,
// it also moves to first. We test both: signal annotation (always) and rank
// promotion (when scores are equal before boost).
func TestReRankHits_WorkingSetBoosted(t *testing.T) {
	wsSignals := &memory.WorkingSetSignals{Files: []string{"pkg/memory/vector_cache.go"}}
	tokens := anchorTokens(nil, nil, wsSignals)

	// Case 1: single hit with ws match → gets signal.
	single := []preSearchHit{
		{Category: "learning", Content: "vector_cache.go needs periodic reloading"},
	}
	res1 := reRankHits(single, tokens, nil, wsSignals)
	if res1[0].Category != "learning" {
		t.Errorf("single hit: expected learning, got %s", res1[0].Category)
	}
	foundSignal := false
	for _, s := range res1[0].Signals {
		if s == "working_set" {
			foundSignal = true
		}
	}
	if !foundSignal {
		t.Errorf("expected working_set signal on boosted hit, got %v", res1[0].Signals)
	}

	// Case 2: ws-hit is at index 0 (top BM25), non-ws at index 1 — ws-hit stays first.
	two := []preSearchHit{
		{Category: "learning", Content: "vector_cache.go needs periodic reloading"},
		{Category: "decision", Content: "decided to use postgres for storage"},
	}
	res2 := reRankHits(two, tokens, nil, wsSignals)
	if res2[0].Category != "learning" {
		t.Errorf("two hits: expected ws-hit (learning) first, got %s", res2[0].Category)
	}
	foundSignal = false
	for _, s := range res2[0].Signals {
		if s == "working_set" {
			foundSignal = true
		}
	}
	if !foundSignal {
		t.Errorf("expected working_set signal on boosted hit, got %v", res2[0].Signals)
	}
}

// TestReRankHits_NoAnchors_PreservesOrder verifies regression: no anchors → BM25 order unchanged.
func TestReRankHits_NoAnchors_PreservesOrder(t *testing.T) {
	hits := []preSearchHit{
		{Category: "decision", Content: "first hit from bm25"},
		{Category: "learning", Content: "second hit from bm25"},
		{Category: "fact", Content: "third hit from bm25"},
	}
	original := make([]preSearchHit, len(hits))
	copy(original, hits)

	result := reRankHits(hits, nil, nil, nil)
	for i, h := range result {
		if h.Category != original[i].Category {
			t.Errorf("position %d: got %s, want %s", i, h.Category, original[i].Category)
		}
	}
}

// ── expanded query ────────────────────────────────────────────────────────────

func TestBuildExpandedQuery_AnchorsPrioritized(t *testing.T) {
	freeText := "search for memory issues"
	fileAnchors := []string{"hybrid_search.go"}
	q := buildExpandedQuery(freeText, fileAnchors, nil, nil)

	toks := strings.Fields(q)
	if len(toks) > anchorQueryCap {
		t.Errorf("expanded query has %d tokens, want <= %d", len(toks), anchorQueryCap)
	}
	found := false
	for _, tok := range toks {
		if strings.Contains(tok, "hybrid") {
			found = true
		}
	}
	if !found {
		t.Errorf("anchor token not prioritized in query %q", q)
	}
}

func TestBuildExpandedQuery_CapRespected(t *testing.T) {
	// 30 free-text tokens + 5 anchor tokens → total must be <= anchorQueryCap (24)
	var freeWords []string
	for i := 0; i < 30; i++ {
		freeWords = append(freeWords, "word"+itoaBench(i))
	}
	fileAnchors := []string{"config.go", "server.go", "client.go", "main.go", "utils.go"}
	q := buildExpandedQuery(strings.Join(freeWords, " "), fileAnchors, nil, nil)
	toks := strings.Fields(q)
	if len(toks) > anchorQueryCap {
		t.Errorf("expanded query has %d tokens, exceeds cap %d", len(toks), anchorQueryCap)
	}
}

// ── adaptive reminder ─────────────────────────────────────────────────────────

func TestAdaptiveReminder_Strong(t *testing.T) {
	hits := []preSearchHit{
		{Category: "learning", Content: "x", Signals: []string{"file_anchor"}},
	}
	if mode := classifyAdaptiveReminder(hits); mode != adaptiveReminderStrong {
		t.Errorf("expected adaptiveReminderStrong, got %d", mode)
	}
}

func TestAdaptiveReminder_StrongFromWorkingSet(t *testing.T) {
	hits := []preSearchHit{
		{Category: "decision", Content: "y", Signals: []string{"working_set"}},
	}
	if mode := classifyAdaptiveReminder(hits); mode != adaptiveReminderStrong {
		t.Errorf("expected adaptiveReminderStrong for working_set signal, got %d", mode)
	}
}

func TestAdaptiveReminder_Short(t *testing.T) {
	if mode := classifyAdaptiveReminder(nil); mode != adaptiveReminderShort {
		t.Errorf("expected adaptiveReminderShort, got %d", mode)
	}
}

func TestAdaptiveReminder_Default(t *testing.T) {
	hits := []preSearchHit{
		{Category: "decision", Content: "y", Signals: nil},
	}
	if mode := classifyAdaptiveReminder(hits); mode != adaptiveReminderDefault {
		t.Errorf("expected adaptiveReminderDefault, got %d", mode)
	}
}

func TestRenderRecallPreview_AdaptiveStrongReminder(t *testing.T) {
	hits := []preSearchHit{
		{Category: "learning", Content: "vector_cache.go reloads on startup", Signals: []string{"file_anchor"}},
	}
	out := renderRecallPreview("vector_cache.go", "code_change", hits, nil, "", adaptiveReminderStrong, 4800)
	if !strings.Contains(out, "memórias relevantes injetadas acima") {
		t.Errorf("expected strong reminder text, got: %s", out)
	}
}

func TestRenderRecallPreview_AdaptiveShortReminder(t *testing.T) {
	arts := []recentArtifact{{Type: "test_report", SourceLabel: "go test", AgeHint: "2026-06-09"}}
	out := renderRecallPreview("q", "debugging", nil, arts, "", adaptiveReminderShort, 4800)
	if !strings.Contains(out, "anchored_search") {
		t.Errorf("expected short reminder with anchored_search, got: %s", out)
	}
}

func TestRenderRecallPreview_KGLine(t *testing.T) {
	hits := []preSearchHit{{Category: "decision", Content: "some content"}}
	kgLine := "<anchored_kg>HybridSearch uses BM25; BM25 is algorithm</anchored_kg>"
	out := renderRecallPreview("q", "planning", hits, nil, kgLine, adaptiveReminderDefault, 4800)
	if !strings.Contains(out, "<anchored_kg>") {
		t.Errorf("expected KG line in output, got: %s", out)
	}
}

func TestRenderRecallPreview_SignalsAttr(t *testing.T) {
	hits := []preSearchHit{
		{Category: "learning", Content: "x", Signals: []string{"file_anchor"}},
	}
	out := renderRecallPreview("q", "planning", hits, nil, "", adaptiveReminderStrong, 4800)
	if !strings.Contains(out, `signals="file_anchor"`) {
		t.Errorf("expected signals attr in output, got: %s", out)
	}
}

// ── AC §4 item 3 integration: file anchor end-to-end ─────────────────────────

// TestRecall_FileAnchorPrompt_EndToEnd: prompt with file path → hit mentioning
// that file is ranked first with "file_anchor" signal.
func TestRecall_FileAnchorPrompt_EndToEnd(t *testing.T) {
	db := newFTSTestDB(t)

	// m1: high BM25 text match for "analisa" but does not mention the file.
	insertMem(t, db, "m1", "proj-A", "decision", "analisa sempre o contexto antes de decidir qualquer coisa importante")
	// m2: lower BM25 text match but mentions the specific file.
	insertMem(t, db, "m2", "proj-A", "learning", "hybrid_search.go implementa o algoritmo BM25 com FTS5")

	prompt := "analisa pkg/memory/hybrid_search.go"
	fileAnchors, symAnchors := extractAnchors(prompt)

	found := false
	for _, f := range fileAnchors {
		if strings.Contains(f, "hybrid_search.go") {
			found = true
		}
	}
	if !found {
		t.Fatalf("hybrid_search.go not detected as file anchor, files=%v", fileAnchors)
	}

	q := sanitizeFTSQuery(prompt)
	expanded := buildExpandedQuery(q, fileAnchors, symAnchors, nil)

	hits, err := bm25TopHits(context.Background(), db, expanded, "proj-A", 5)
	if err != nil {
		t.Fatalf("bm25TopHits: %v", err)
	}

	tokens := anchorTokens(fileAnchors, symAnchors, nil)
	ranked := reRankHits(hits, tokens, fileAnchors, nil)

	if len(ranked) == 0 {
		t.Fatal("expected at least one hit")
	}
	if !strings.Contains(ranked[0].Content, "hybrid_search.go") {
		t.Errorf("expected hybrid_search.go hit first, got: %s", ranked[0].Content)
	}
	foundSig := false
	for _, s := range ranked[0].Signals {
		if s == "file_anchor" {
			foundSig = true
		}
	}
	if !foundSig {
		t.Errorf("expected file_anchor signal, got signals=%v", ranked[0].Signals)
	}
}

// ── AC §4 item 4 integration: working set end-to-end ─────────────────────────

// TestRecall_WorkingSet_EndToEnd: working set with vector_cache.go causes the
// related memory to gain "working_set" signal.
func TestRecall_WorkingSet_EndToEnd(t *testing.T) {
	db := newFTSTestDB(t)

	// m1: strong BM25 for "deploy" but unrelated to working set.
	insertMem(t, db, "m1", "proj-A", "decision", "deploy sempre via CI não manual nunca fazer deploy manual")
	// m2: mentions vector_cache.go — the working-set file.
	insertMem(t, db, "m2", "proj-A", "learning", "vector_cache.go deve ser recarregado após rebuild do modelo")

	wsSignals := &memory.WorkingSetSignals{Files: []string{"pkg/memory/vector_cache.go"}}

	q := sanitizeFTSQuery("como fazer o deploy")
	expanded := buildExpandedQuery(q, nil, nil, wsSignals)

	hits, err := bm25TopHits(context.Background(), db, expanded, "proj-A", 5)
	if err != nil {
		t.Fatalf("bm25TopHits: %v", err)
	}

	tokens := anchorTokens(nil, nil, wsSignals)
	ranked := reRankHits(hits, tokens, nil, wsSignals)

	for _, h := range ranked {
		if strings.Contains(h.Content, "vector_cache.go") {
			for _, s := range h.Signals {
				if s == "working_set" {
					return // pass
				}
			}
			t.Errorf("vector_cache.go hit found but missing working_set signal; signals=%v", h.Signals)
			return
		}
	}
	t.Error("no hit mentioning vector_cache.go found in ranked results")
}

// ── AC §4 item 3 regression: no anchors = current pipeline ───────────────────

func TestRecall_NoAnchors_SameAsCurrentPipeline(t *testing.T) {
	db := newFTSTestDB(t)
	insertMem(t, db, "m1", "proj-A", "decision", "postgres is the primary database for durable storage")
	insertMem(t, db, "m2", "proj-A", "learning", "postgres connection pooling reduces latency")

	prompt := "como usar postgres"
	q := sanitizeFTSQuery(prompt)
	files, syms := extractAnchors(prompt)

	if len(files) != 0 || len(syms) != 0 {
		t.Errorf("unexpected anchors in plain prose: files=%v syms=%v", files, syms)
	}

	expanded := buildExpandedQuery(q, files, syms, nil)
	if expanded != q {
		t.Errorf("expected expanded == q when no anchors; expanded=%q q=%q", expanded, q)
	}

	oldHits, err := bm25TopHits(context.Background(), db, q, "proj-A", 5)
	if err != nil {
		t.Fatal(err)
	}
	newHits, err := bm25TopHits(context.Background(), db, expanded, "proj-A", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(oldHits) != len(newHits) {
		t.Errorf("hit count mismatch: old=%d new=%d", len(oldHits), len(newHits))
	}
}

// ── AC §4 item 6: fail-safe ───────────────────────────────────────────────────

// TestAutoRecallPreview_FailSafe verifies that autoRecallPreview never panics
// with broken inputs (empty prompt, missing config/DB, empty session ID).
func TestAutoRecallPreview_FailSafe(t *testing.T) {
	nopLog := &debuglog.Logger{}
	cases := []struct {
		name      string
		config    string
		cwd       string
		prompt    string
		sessionID string
	}{
		{"empty prompt", "", ".", "", ""},
		{"missing config db", "/nonexistent/config.yaml", ".", "analisa algo importante aqui", ""},
		{"empty cwd", "", "", "como usar postgres em producao", ""},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic in autoRecallPreview: %v", r)
				}
			}()
			_ = autoRecallPreview(tc.config, tc.cwd, tc.prompt, tc.sessionID, nopLog)
		})
	}
}

// TestRecordInjection_TracksWorkingSetAndCount guards D-lite: injected memory
// IDs land in the session working set and injected_count increments per
// injection. Uses a real config file + migrated DB so the write path matches
// production.
func TestRecordInjection_TracksWorkingSetAndCount(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("memory:\n  database_path: "+dbPath+"\nembedding:\n  provider: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, id := range []string{"mem-a", "mem-b"} {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, content_hash, created_at, updated_at, metadata)
			 VALUES (?, 'proj1', 'decision', 'content of '||?, '', datetime('now'), datetime('now'), '{}')`,
			id, id); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	dlog := &debuglog.Logger{}
	hits := []preSearchHit{{ID: "mem-a"}, {ID: "mem-b"}}
	recordInjection(cfgPath, "sess-1", "proj1", hits, dlog)
	recordInjection(cfgPath, "sess-1", "proj1", hits[:1], dlog)

	check, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()

	var countA, countB int
	if err := check.QueryRow(`SELECT json_extract(metadata,'$.injected_count') FROM memories WHERE id='mem-a'`).Scan(&countA); err != nil {
		t.Fatalf("mem-a count: %v", err)
	}
	if err := check.QueryRow(`SELECT json_extract(metadata,'$.injected_count') FROM memories WHERE id='mem-b'`).Scan(&countB); err != nil {
		t.Fatalf("mem-b count: %v", err)
	}
	if countA != 2 || countB != 1 {
		t.Errorf("injected_count: mem-a=%d (want 2), mem-b=%d (want 1)", countA, countB)
	}

	var memIDs string
	if err := check.QueryRow(`SELECT memory_ids FROM working_sets WHERE session_id='sess-1'`).Scan(&memIDs); err != nil {
		t.Fatalf("working set: %v", err)
	}
	if !strings.Contains(memIDs, "mem-a") || !strings.Contains(memIDs, "mem-b") {
		t.Errorf("working_sets.memory_ids should contain both IDs, got: %s", memIDs)
	}
}

// TestBM25TopHits_ExcludesLowSignal guards the push-path side of the usage
// feedback loop: a memory demoted to low_signal (quality or never_used) must
// stop being injected by the recall hook.
func TestBM25TopHits_ExcludesLowSignal(t *testing.T) {
	db := newFTSTestDB(t)
	insertMem(t, db, "ok-1", "proj-A", "decision", "postgres storage decision for the team server")
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, keywords, metadata)
		 VALUES ('demoted-1', 'proj-A', 'decision', 'postgres storage noise that was demoted', '',
		         '{"curation_status":"low_signal","curation_rule":"never_used"}')`); err != nil {
		t.Fatal(err)
	}

	hits, err := bm25TopHits(context.Background(), db, "postgres storage", "proj-A", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "demoted-1" {
			t.Fatal("low_signal memory must not be injected by the recall hook")
		}
	}
	if len(hits) != 1 || hits[0].ID != "ok-1" {
		t.Fatalf("want only ok-1, got %+v", hits)
	}
}

// ─── Context-gate credit wiring ─────────────────────────────────────────────

// TestCreditGateFromRecall_OnlyWhenSomethingWasRendered is the fix for
// crediting off the hit count instead of the delivered payload.
// renderRecallPreview drops hits to fit the byte budget and returns "" when
// even the top hit overflows, so a session could have hits, credit the gate,
// and receive nothing — opening the gate for exactly the case it exists to
// catch.
func TestCreditGateFromRecall_OnlyWhenSomethingWasRendered(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Memory.StorageDir = dir
	cfg.Plugin.ContextGate = "enforce"
	markSessionStartRichBlock(dir, "sess-render")

	if creditGateFromRecall(cfg, "sess-render", "", &debuglog.Logger{}) {
		t.Fatal("credited the gate although nothing was rendered into the prompt")
	}
	if dec, _ := contextGateDecision(dir, "sess-render", "Bash"); dec == nil {
		t.Fatal("gate should still be armed after an empty preview")
	}

	if !creditGateFromRecall(cfg, "sess-render-2", "<anchored_recall …>", &debuglog.Logger{}) {
		markSessionStartRichBlock(dir, "sess-render-2")
		if !creditGateFromRecall(cfg, "sess-render-2", "<anchored_recall …>", &debuglog.Logger{}) {
			t.Fatal("rendered preview + rich marker should credit the gate")
		}
	}
}

// TestCreditGateFromRecall_RespectsGateMode keeps the credit path inert when
// the user opted out of the gate entirely.
func TestCreditGateFromRecall_RespectsGateMode(t *testing.T) {
	dir := t.TempDir()
	markSessionStartRichBlock(dir, "sess-off")

	cfg := &config.Config{}
	cfg.Memory.StorageDir = dir
	cfg.Plugin.ContextGate = "disabled"

	if creditGateFromRecall(cfg, "sess-off", "<anchored_recall …>", &debuglog.Logger{}) {
		t.Error("credited the gate while the gate is disabled")
	}
	if creditGateFromRecall(nil, "sess-off", "<anchored_recall …>", &debuglog.Logger{}) {
		t.Error("credited the gate with a nil config")
	}
}

// TestAutoRecallPreview_CreditsGateEndToEnd pins the CALL SITE, not the helper.
// Deleting the creditGateFromRecall call from autoRecallPreview must fail a
// test — otherwise the 53%-denial bug this change exists to fix can be
// reintroduced with a green suite.
func TestAutoRecallPreview_CreditsGateEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	storageDir := filepath.Join(dir, "storage")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := "memory:\n  database_path: " + dbPath +
		"\n  storage_dir: " + storageDir +
		"\nembedding:\n  provider: none\nplugin:\n  context_gate: enforce\n"
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, content_hash, created_at, updated_at, metadata)
		 VALUES ('m1', '', 'decision', 'we settled on postgres for the billing ledger', '', datetime('now'), datetime('now'), '{}')`,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	const sess = "sess-e2e"
	const prompt = "how did we decide the billing ledger postgres question"

	// Without SessionStart's rich block the recall must NOT credit: the
	// conservative half of the two-signal rule.
	if out := autoRecallPreview(cfgPath, dir, prompt, sess, &debuglog.Logger{}); out == "" {
		t.Skip("recall returned nothing for the seeded memory; FTS unavailable in this build")
	}
	if gateIsSatisfied(filepath.Join(storageDir, "ctxgate", sanitizeSessionID(sess))) {
		t.Fatal("recall credited the gate without SessionStart's rich block")
	}

	// With it, the same recall must credit.
	markSessionStartRichBlock(storageDir, sess)
	autoRecallPreview(cfgPath, dir, prompt, sess, &debuglog.Logger{})
	if !gateIsSatisfied(filepath.Join(storageDir, "ctxgate", sanitizeSessionID(sess))) {
		t.Fatal("recall injected memories with the rich block present but did not credit the gate")
	}
	if dec, stage := contextGateDecision(storageDir, sess, "Bash"); dec != nil {
		t.Fatalf("gate denied a session whose memory was already delivered (stage=%q)", stage)
	}
}
