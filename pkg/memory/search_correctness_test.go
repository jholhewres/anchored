package memory

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openSearchTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "search.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// FTS5's bm25() is more negative for a better match. The store returns rows
// best-first, so the scores it reports must not increase down the list: the
// fusion normalizes by the list's max and would otherwise rank the weakest
// lexical match highest.
func TestBM25ScoreMonotonic(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	for id, content := range map[string]string{
		"strong": "deploy deploy deploy pipeline deploy",
		"medium": "the deploy pipeline",
		"weak":   "a long note about many unrelated things where deploy shows up once among other words and more words",
	} {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := store.Search(ctx, "deploy", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 || res[0].Memory.ID != "strong" {
		t.Fatalf("expected strong first of 3, got %v", resultIDs(res))
	}
	for i := 1; i < len(res); i++ {
		if res[i].Score > res[i-1].Score {
			t.Fatalf("score rises down the ranking: %v", resultScores(res))
		}
	}
	if res[0].Score <= res[len(res)-1].Score {
		t.Fatalf("best match must score above the weakest: %v", resultScores(res))
	}
}

// A project-scoped search also returns global memories (no project): user
// preferences and facts that hold everywhere.
func TestStoreSearch_ProjectScopeIncludesGlobals(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	a, b := "proj-a", "proj-b"
	for _, m := range []Memory{
		{ID: "in-a", ProjectID: &a, Category: "fact", Content: "deploy runs from the release branch"},
		{ID: "in-b", ProjectID: &b, Category: "fact", Content: "deploy uses blue green"},
		{ID: "global", Category: "preference", Content: "deploy only after the tests pass"},
	} {
		if err := store.Save(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	res, err := store.Search(ctx, "deploy", SearchOptions{MaxResults: 10, ProjectID: a})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range res {
		got[r.Memory.ID] = true
	}
	if !got["in-a"] || !got["global"] || got["in-b"] {
		t.Fatalf("scope %s returned %v, want in-a and global only", a, resultIDs(res))
	}
}

type fixedQueryEmbedder struct{ vec []float32 }

func (e *fixedQueryEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = e.vec
	}
	return out, nil
}
func (e *fixedQueryEmbedder) Dimensions() int { return len(e.vec) }
func (e *fixedQueryEmbedder) Name() string    { return "fixed" }
func (e *fixedQueryEmbedder) Model() string   { return "fixed" }
func (e *fixedQueryEmbedder) Close() error    { return nil }

// A small project inside a large corpus: the vector top-k must be taken
// within the scope, not globally and then filtered, or the 5,000 memories of
// another project that sit closer to the query crowd the small one out.
func TestHybridSearch_ScopedVectorSearchFindsASmallProject(t *testing.T) {
	big, small := "proj-big", "proj-small"
	memories := map[string]Memory{}
	vc := NewVectorCache(nil)
	now := time.Now()
	for i := 0; i < 5000; i++ {
		id := fmt.Sprintf("big-%04d", i)
		memories[id] = Memory{ID: id, ProjectID: &big, Category: "fact", Content: "unrelated", CreatedAt: now}
		vc.Put(id, []float32{1, 0.01 * float32(i%7), 0, 0})
		vc.SetScope(id, big)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("small-%02d", i)
		memories[id] = Memory{ID: id, ProjectID: &small, Category: "fact", Content: "unrelated", CreatedAt: now}
		vc.Put(id, []float32{1, 1, float32(i) * 0.01, 0})
		vc.SetScope(id, small)
	}
	memories["global"] = Memory{ID: "global", Category: "preference", Content: "unrelated", CreatedAt: now}
	// cos 0.98 to the query: ahead of every small-project memory (≤ 0.71)
	// even after the in-project boost (×1.3 against ×1.1 for globals).
	vc.Put("global", []float32{1, 0.2, 0, 0})
	vc.SetScope("global", "")

	cfg := DefaultHybridSearchConfig()
	cfg.MMREnabled = false
	cfg.TemporalDecayEnabled = false
	cfg.DiversifyPerOrigin = 0
	h := NewHybridSearcher(&hybridMockStore{memories: memories}, &fixedQueryEmbedder{vec: []float32{1, 0, 0, 0}}, nil, vc, cfg, nil, nil, nil)

	res, err := h.Search(context.Background(), "q", SearchOptions{MaxResults: 10, ProjectID: small})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 10 {
		t.Fatalf("scoped search returned %d results, want 10: %v", len(res), resultIDs(res))
	}
	sawGlobal := false
	for _, r := range res {
		if r.Memory.ProjectID != nil && *r.Memory.ProjectID == big {
			t.Fatalf("result %s is outside the scope", r.Memory.ID)
		}
		sawGlobal = sawGlobal || r.Memory.ID == "global"
	}
	if !sawGlobal {
		t.Errorf("the global memory is closer to the query than any small-project one and must be returned: %v", resultIDs(res))
	}
}

func equalScoreFixture(n int, sameOrigin int) (map[string]Memory, *VectorCache) {
	memories := map[string]Memory{}
	vc := NewVectorCache(nil)
	now := time.Now()
	session := "prolix-session"
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m-%02d", i)
		m := Memory{ID: id, Category: "fact", Content: fmt.Sprintf("note number %d about the cache", i), CreatedAt: now}
		if i < sameOrigin {
			m.SourceID = &session
		} else {
			other := fmt.Sprintf("session-%02d", i)
			m.SourceID = &other
		}
		memories[id] = m
		// e0 + e(i+1): every candidate has the same cosine to the query (e0)
		// and 0.5 to every other candidate, so they tie without being
		// near-duplicates of each other.
		vec := make([]float32, n+1)
		vec[0], vec[i+1] = 1, 1
		vc.Put(id, vec)
	}
	return memories, vc
}

func unitQuery(dims int) []float32 {
	q := make([]float32, dims)
	q[0] = 1
	return q
}

// The same search against the same data returns the same ranking. MMR used to
// start from an arbitrary candidate (a slice built from a map), so ties were
// broken differently on every call.
func TestHybridSearch_IsDeterministic(t *testing.T) {
	memories, vc := equalScoreFixture(30, 0)
	cfg := DefaultHybridSearchConfig()
	cfg.TemporalDecayEnabled = false
	h := NewHybridSearcher(&hybridMockStore{memories: memories}, &fixedQueryEmbedder{vec: unitQuery(31)}, nil, vc, cfg, nil, nil, nil)

	first := ""
	for i := 0; i < 100; i++ {
		res, err := h.Search(context.Background(), "cache", SearchOptions{MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprint(resultIDs(res))
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d returned %s, run 0 returned %s", i, got, first)
		}
	}
}

// Diversification caps a prolix origin, and the cut to k happens after it:
// with 13 eligible candidates (3 allowed from the prolix session + 10 others)
// a k=10 search returns 10, not the handful left after capping a top-10.
func TestHybridSearch_ReturnsKWhenEnoughCandidatesSurviveDiversification(t *testing.T) {
	memories, vc := equalScoreFixture(30, 20)
	cfg := DefaultHybridSearchConfig()
	cfg.TemporalDecayEnabled = false
	cfg.DiversifyPerOrigin = 3
	h := NewHybridSearcher(&hybridMockStore{memories: memories}, &fixedQueryEmbedder{vec: unitQuery(31)}, nil, vc, cfg, nil, nil, nil)

	res, err := h.Search(context.Background(), "cache", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 10 {
		t.Fatalf("got %d results, want 10: %v", len(res), resultIDs(res))
	}
	fromProlix := 0
	for _, r := range res {
		if r.Memory.SourceID != nil && *r.Memory.SourceID == "prolix-session" {
			fromProlix++
		}
	}
	if fromProlix > 3 {
		t.Errorf("%d results from one session, cap is 3", fromProlix)
	}
}

// One decay: exponential in the time since the memory was created or last
// used, whichever is later, with the category's half-life. Pinned and demoted
// memories do not decay. It used to stack a second, banded decay on top and
// ignore recent use.
func TestTemporalDecay_IsAppliedOnceFromLastUse(t *testing.T) {
	now := time.Now()
	cfg := DefaultHybridSearchConfig()
	h := &HybridSearcher{config: cfg}
	old := now.Add(-200 * 24 * time.Hour)
	recentUse := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		m    Memory
		want float64
	}{
		{"old event, never used", Memory{ID: "a", Category: "event", CreatedAt: old}, math.Exp(-math.Ln2 / 30 * 200)},
		{"old event, used yesterday", Memory{ID: "b", Category: "event", CreatedAt: old,
			Metadata: map[string]any{"last_used_at": recentUse}}, math.Exp(-math.Ln2 / 30 * 1)},
		{"old decision", Memory{ID: "c", Category: "decision", CreatedAt: old}, math.Exp(-math.Ln2 / 180 * 200)},
		{"old pinned event", Memory{ID: "d", Category: "event", CreatedAt: old,
			Metadata: map[string]any{"pinned": true}}, 1.5},
	}
	for _, c := range cases {
		res := []SearchResult{{Memory: c.m, Score: 1}}
		res = applyLifecycleBoost(res, now)
		res = h.applyTemporalDecay(res, cfg)
		if math.Abs(res[0].Score-c.want) > 0.01 {
			t.Errorf("%s: score %.4f, want %.4f", c.name, res[0].Score, c.want)
		}
	}
}

func resultIDs(res []SearchResult) []string {
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.Memory.ID
	}
	return ids
}

func resultScores(res []SearchResult) []float64 {
	s := make([]float64, len(res))
	for i, r := range res {
		s[i] = r.Score
	}
	return s
}

// The store is what keeps the cache's scopes right: in bulk when it loads a
// generation, and one memory at a time when a vector is written.
func TestVectorScopesFollowTheStore(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	a := "proj-a"
	for _, m := range []Memory{
		{ID: "in-a", ProjectID: &a, Category: "fact", Content: "alpha"},
		{ID: "global", Category: "fact", Content: "beta"},
	} {
		if err := store.Save(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	vc := store.VectorCache()
	for _, id := range []string{"in-a", "global", "other"} {
		vc.Put(id, []float32{1, 0})
	}
	vc.SetScope("other", "proj-b")
	if err := store.refreshVectorScopes(ctx); err != nil {
		t.Fatal(err)
	}
	// "other" is not a live memory: after the bulk refresh its scope is
	// unknown, so it competes everywhere and the per-memory filter decides.
	got := map[string]bool{}
	for _, s := range vc.ScoreInScope([]float32{1, 0}, 1, 0, 10, "proj-z") {
		got[s.ID] = true
	}
	if got["in-a"] || !got["global"] || !got["other"] {
		t.Fatalf("scope proj-z scored %v", got)
	}
	store.refreshVectorScope(ctx, "in-a")
	if s := vc.ScoreInScope([]float32{1, 0}, 1, 0, 10, a); len(s) != 3 {
		t.Fatalf("scope %s scored %v, want in-a, global and the unknown one", a, s)
	}
}

// A weak covering hit (a long memory citing each word once) must not push
// the OR fill under the relevance cutoff: BM25-only search still returns k.
func TestSearchBM25_WeakCoveringHitDoesNotSquashTheORFill(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	long := "a long note about the week: " + strings.Repeat("filler words about many unrelated topics ", 12) + "deploy " + strings.Repeat("more filler ", 12) + "banco"
	mems := map[string]string{"and-strong": "deploy banco deploy banco schema", "and-weak": long}
	services := []string{"billing", "search", "gateway", "mailer", "scheduler", "exporter", "importer", "profiles", "catalog", "checkout", "invoices", "reports", "webhooks"}
	for i, svc := range services {
		mems[fmt.Sprintf("or-%02d", i)] = fmt.Sprintf("deploy checklist for the %s service", svc)
	}
	// Unrelated memories, so the query words are rare enough to carry weight.
	for i := 0; i < 40; i++ {
		mems[fmt.Sprintf("other-%02d", i)] = fmt.Sprintf("unrelated memory %d about the office kitchen", i)
	}
	for id, c := range mems {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultHybridSearchConfig()
	cfg.TemporalDecayEnabled = false
	cfg.DiversifyPerOrigin = 0 // same-day memories without a session share one origin
	h := NewHybridSearcher(store, nil, nil, nil, cfg, nil, nil, nil)
	res, err := h.Search(ctx, "deploy banco", SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 10 {
		t.Fatalf("got %d results, want 10: %v", len(res), resultIDs(res))
	}
	if res[0].Memory.ID != "and-strong" {
		t.Errorf("the strong covering hit ranks first, got %v", resultIDs(res))
	}
}

// Covering the query is a boost on the fused score, not a rescaling of the
// lexical list: the other candidates keep their scores.
func TestApplyCoverageBoost_OnlyTouchesCoveringHits(t *testing.T) {
	res := []SearchResult{{Memory: Memory{ID: "a"}, Score: 0.2}, {Memory: Memory{ID: "o"}, Score: 0.3}}
	applyCoverageBoost(res, map[string]bool{"a": true})
	if res[0].Score != 0.2*coverageBoost || res[1].Score != 0.3 {
		t.Fatalf("scores after the boost: %v", resultScores(res))
	}
}

// Demotion multiplies a weak memory's score down; decay must still apply on
// top, or a demoted memory ends above a legitimate one of the same age.
func TestTemporalDecay_DemotedMemoriesStillDecay(t *testing.T) {
	now := time.Now()
	cfg := DefaultHybridSearchConfig()
	h := &HybridSearcher{config: cfg}
	old := now.Add(-100 * 24 * time.Hour)
	res := []SearchResult{
		{Memory: Memory{ID: "legit", Category: "plan", CreatedAt: old}, Score: 1},
		{Memory: Memory{ID: "demoted", Category: "plan", CreatedAt: old, Metadata: map[string]any{"quality_score": 0.1}}, Score: 1},
	}
	res = applyLifecycleBoost(res, now)
	res = h.applyTemporalDecay(res, cfg)
	if res[1].Score >= res[0].Score {
		t.Fatalf("demoted %.4f must stay below legitimate %.4f", res[1].Score, res[0].Score)
	}
}

// Punctuated words (file names, versions, hyphenated terms) must not break the
// MATCH expression: a broken one fell back to an OR of every token, including
// the literal words AND/OR, and ranked memories full of "and"/"or" first.
func TestSearchBM25_PunctuatedWordsKeepTheQueryIntact(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	for id, c := range map[string]string{
		"target": "node.js error handling uses a central middleware",
		"junk":   "this and that or those and these or them and more",
		"yaml":   "config.yaml holds the deploy settings",
	} {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultHybridSearchConfig()
	cfg.TemporalDecayEnabled = false
	h := NewHybridSearcher(store, nil, nil, nil, cfg, nil, nil, nil)
	for q, want := range map[string]string{"node.js error handling": "target", "config.yaml deploy": "yaml", "foo-bar database handling": "target"} {
		res, err := h.Search(ctx, q, SearchOptions{MaxResults: 5})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if r.Memory.ID == "junk" {
				t.Errorf("%q matched a memory that only shares the words and/or: %v", q, resultIDs(res))
			}
		}
		if len(res) == 0 || res[0].Memory.ID != want {
			t.Errorf("%q: got %v, want %s first", q, resultIDs(res), want)
		}
	}
}

// NEAR/n as users type it becomes FTS5's NEAR(a b, n) and matches.
func TestSearchBM25_NEARIsValidFTS5(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Memory{ID: "near", Category: "fact", Content: "the auth flow ends on the login page"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Search(ctx, ExpandQueryAdvanced("auth NEAR/5 login"), SearchOptions{MaxResults: 5}); err != nil {
		t.Fatal(err)
	}
	res, err := store.Search(ctx, `NEAR("auth" "login", 5)`, SearchOptions{MaxResults: 5})
	if err != nil || len(res) != 1 {
		t.Fatalf("FTS5 NEAR: %v %v", resultIDs(res), err)
	}
}

func TestSafeFTSOr_DropsOperatorWords(t *testing.T) {
	got := safeFTSOr(`"node" OR node* AND "js" NEAR/5 x NOT y`)
	for _, op := range []string{`"OR"`, `"AND"`, `"NEAR"`, `"NOT"`} {
		if strings.Contains(got, op) {
			t.Errorf("safe OR kept operator %s: %s", op, got)
		}
	}
	if strings.Count(got, `"node"`) != 1 {
		t.Errorf("safe OR must deduplicate: %s", got)
	}
}

// The heap cut is deterministic with more ties than k.
func TestScoreInScope_TiesBeyondKAreCutByID(t *testing.T) {
	vc := NewVectorCache(nil)
	for i := 0; i < 50; i++ {
		vc.Put(fmt.Sprintf("m-%02d", i), []float32{1, 0})
	}
	for run := 0; run < 50; run++ {
		got := vc.ScoreInScope([]float32{1, 0}, 1, 0, 5, "")
		ids := make([]string, len(got))
		for i, s := range got {
			ids[i] = s.ID
		}
		if fmt.Sprint(ids) != "[m-00 m-01 m-02 m-03 m-04]" {
			t.Fatalf("run %d cut the ties as %v", run, ids)
		}
	}
}

// Vector writes after the load record the memory's scope (the job path and
// the generation path), so a new memory competes only in its own project.
func TestVectorScopes_FollowWritesAfterTheLoad(t *testing.T) {
	ctx := context.Background()
	store := openSearchTestStore(t)
	q := "proj-q"
	identity := EmbeddingIdentity{Provider: "bow", Model: "m", ModelRevision: "r", Dimensions: 2, Normalization: "l2"}
	gen, err := store.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Memory{ID: "new", ProjectID: &q, Category: "fact", Content: "fresh memory"}); err != nil {
		t.Fatal(err)
	}
	revs, err := store.ListMissingEmbeddingRevisions(ctx, gen.ID, 10)
	if err != nil || len(revs) != 1 {
		t.Fatalf("missing revisions %v %v", revs, err)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{RevisionID: revs[0].RevisionID, MemoryID: "new",
		GenerationID: gen.ID, Purpose: EmbeddingPurposeDocument, Identity: identity,
		ContentHash: revs[0].Memory.ContentHash, Vector: []float32{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if got := store.VectorCache().ScoreInScope([]float32{1, 0}, 1, 0, 5, "proj-other"); len(got) != 0 {
		t.Fatalf("a memory of %s competed in another scope: %v", q, got)
	}
	if got := store.VectorCache().ScoreInScope([]float32{1, 0}, 1, 0, 5, q); len(got) != 1 {
		t.Fatalf("scope %s: %v", q, got)
	}

	// The legacy-column job path.
	if err := store.Save(ctx, Memory{ID: "legacy-path", ProjectID: &q, Category: "fact", Content: "another one"}); err != nil {
		t.Fatal(err)
	}
	var revID string
	if err := store.DB().QueryRowContext(ctx, `SELECT current_revision_id FROM memories WHERE id = ?`, "legacy-path").Scan(&revID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateEmbeddingForRevision(ctx, "legacy-path", revID, []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	for _, s := range store.VectorCache().ScoreInScope([]float32{1, 0}, 1, 0, 5, "proj-other") {
		if s.ID == "legacy-path" {
			t.Fatal("the job path left the scope unknown")
		}
	}
}

// The expansions are valid FTS5 as generated: no reliance on the retry that
// reduces a broken expression to an OR of its tokens.
func TestExpansions_AreValidFTS5(t *testing.T) {
	store := openSearchTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Memory{ID: "x", Category: "fact", Content: "node.js config.yaml foo-bar sqlite-vec a,b,c"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"node.js error handling", "config.yaml deploy", "foo-bar database", "sqlite-vec vs fts5", "a,b,c values", "auth NEAR/3 login"} {
		for _, expr := range []string{ExpandQueryAdvanced(q), ExpandQueryAND(q)} {
			if expr == "" {
				continue
			}
			var n int
			if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memories_fts WHERE memories_fts MATCH ?`, expr).Scan(&n); err != nil {
				t.Errorf("%q → %s: %v", q, expr, err)
			}
		}
	}
}
