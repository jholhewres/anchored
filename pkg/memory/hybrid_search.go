package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type HybridSearchConfig struct {
	VectorWeight              float64
	BM25Weight                float64
	MaxResults                int
	MinScore                  float64
	MMREnabled                bool
	MMRLambda                 float64
	TemporalDecayEnabled      bool
	TemporalDecayHalfLifeDays int
	// DiversifyPerOrigin caps how many results may come from the same origin
	// (source session, or creation day when the session is unknown) so a single
	// prolix session can't dominate the top-k. 0 disables the cap.
	DiversifyPerOrigin int
}

func DefaultHybridSearchConfig() HybridSearchConfig {
	return HybridSearchConfig{
		VectorWeight:              0.7,
		BM25Weight:                0.3,
		MaxResults:                20,
		MinScore:                  0.05,
		MMREnabled:                true,
		MMRLambda:                 0.7,
		TemporalDecayEnabled:      true,
		TemporalDecayHalfLifeDays: 30,
		DiversifyPerOrigin:        3,
	}
}

type HybridSearcher struct {
	store               Store
	embedder            EmbeddingProvider
	cache               *EmbeddingCache
	vectorCache         *VectorCache
	config              HybridSearchConfig
	entityDetector      *EntityDetector
	topicChangeDetector *TopicChangeDetector
	logger              *slog.Logger
	generationMu        sync.RWMutex
	generationAware     bool
	activeIdentity      *EmbeddingIdentity
}

func NewHybridSearcher(store Store, embedder EmbeddingProvider, cache *EmbeddingCache, vectorCache *VectorCache, cfg HybridSearchConfig, entityDetector *EntityDetector, topicChangeDetector *TopicChangeDetector, logger *slog.Logger) *HybridSearcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &HybridSearcher{store: store, embedder: embedder, cache: cache, vectorCache: vectorCache, config: cfg, entityDetector: entityDetector, topicChangeDetector: topicChangeDetector, logger: logger}
}

// UseEmbeddingGeneration switches semantic search to the generation-aware
// contract. A nil identity intentionally disables vectors (BM25-only) while a
// compatible generation is being built.
func (h *HybridSearcher) UseEmbeddingGeneration(identity *EmbeddingIdentity) {
	h.generationMu.Lock()
	h.generationAware = true
	h.activeIdentity = nil
	if identity != nil {
		copy := *identity
		h.activeIdentity = &copy
	}
	h.generationMu.Unlock()
}

func (h *HybridSearcher) publishEmbeddingGeneration(
	identity EmbeddingIdentity,
	publish func(func(map[string][]float32) error) error,
) error {
	h.generationMu.Lock()
	defer h.generationMu.Unlock()
	err := publish(func(vectors map[string][]float32) error {
		if h.vectorCache != nil {
			h.vectorCache.Replace(vectors)
		}
		copy := identity
		h.generationAware = true
		h.activeIdentity = &copy
		return nil
	})
	return err
}

func (h *HybridSearcher) Search(ctx context.Context, query string, opts ...SearchOptions) ([]SearchResult, error) {
	var searchOpts SearchOptions
	if len(opts) > 0 {
		searchOpts = opts[0]
	}
	cfg := h.config
	// The per-call option wins; the config value is only the default.
	maxResults := searchOpts.MaxResults
	if maxResults <= 0 {
		maxResults = cfg.MaxResults
	}
	if maxResults <= 0 {
		maxResults = 20
	}

	var queryEntities []string
	if h.entityDetector != nil {
		queryEntities = h.entityDetector.Detect(query)
	}

	vecResults, vecErr := h.searchVector(ctx, query, maxResults*4, queryEntities, searchOpts)
	bm25Results, bm25Err := h.searchBM25(ctx, query, maxResults*4, queryEntities, searchOpts)

	if vecErr != nil {
		h.logger.Warn("vector search failed, using BM25 only", "error", vecErr)
	}
	if bm25Err != nil {
		h.logger.Warn("BM25 search failed, using vector only", "error", bm25Err)
	}

	fused := h.fuse(vecResults, bm25Results, cfg.VectorWeight, cfg.BM25Weight)

	fused = applyLifecycleBoost(fused, time.Now())

	fused = h.applyTemporalDecay(fused, cfg)

	if searchOpts.BoostProjectID != "" {
		fused = h.applyProjectBoost(fused, searchOpts.BoostProjectID)
	} else if searchOpts.ProjectID != "" {
		fused = h.applyProjectBoost(fused, searchOpts.ProjectID)
	}

	if !searchOpts.WorkingSet.Empty() {
		fused = applyWorkingSetBoost(fused, searchOpts.WorkingSet, searchOpts.ExplainSignals)
	}

	if searchOpts.ExplainSignals {
		boostPID := searchOpts.BoostProjectID
		if boostPID == "" {
			boostPID = searchOpts.ProjectID
		}
		annotateBaseSignals(fused, boostPID, time.Now())
	}

	if cfg.MMREnabled {
		mmrLambda := cfg.MMRLambda
		if h.topicChangeDetector != nil {
			changed, _ := h.topicChangeDetector.Check(ctx, query)
			if changed {
				mmrLambda = 0.9
			}
		}
		fused = h.applyMMR(fused, mmrLambda, maxResults)
	}

	sort.Slice(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	// Diversify by origin before truncation so one prolix session can't fill
	// the top-k. Runs on the score-sorted list, so the highest-scoring result
	// from each origin is the one kept.
	if cfg.DiversifyPerOrigin > 0 {
		fused = diversifyByOrigin(fused, cfg.DiversifyPerOrigin)
	}

	if len(fused) > maxResults {
		fused = fused[:maxResults]
	}

	return fused, nil
}

func (h *HybridSearcher) semanticSearchEnabled() bool {
	if h.embedder == nil {
		return false
	}
	h.generationMu.RLock()
	enabled := !h.generationAware || h.activeIdentity != nil
	h.generationMu.RUnlock()
	return enabled
}

// resultOrigin identifies which session (or, lacking one, which day) produced a
// memory, so diversification can cap how many come from the same source.
func resultOrigin(m Memory) string {
	if m.SourceID != nil && *m.SourceID != "" {
		return "sid:" + *m.SourceID
	}
	return "day:" + m.CreatedAt.Format("2006-01-02")
}

// diversifyByOrigin keeps at most perOrigin results per origin, preserving the
// input order (which the caller has already sorted by score). A perOrigin <= 0
// is treated as "no cap" by the caller, so this always enforces a real limit.
func diversifyByOrigin(results []SearchResult, perOrigin int) []SearchResult {
	counts := make(map[string]int, len(results))
	out := results[:0:0]
	for _, r := range results {
		origin := resultOrigin(r.Memory)
		if counts[origin] >= perOrigin {
			continue
		}
		counts[origin]++
		out = append(out, r)
	}
	return out
}

func (h *HybridSearcher) searchVector(ctx context.Context, query string, maxResults int, queryEntities []string, opts SearchOptions) ([]SearchResult, error) {
	if h.embedder == nil {
		return nil, nil
	}
	h.generationMu.RLock()
	generationAware := h.generationAware
	var identity *EmbeddingIdentity
	if h.activeIdentity != nil {
		copy := *h.activeIdentity
		identity = &copy
	}
	if generationAware {
		// Hold the semantic-space read lock through query embedding and cache
		// scoring. Activation publishes cache + identity under the write lock,
		// so a query observes either the complete old space or the complete new
		// space, never an identity/cache mixture.
		defer h.generationMu.RUnlock()
	} else {
		h.generationMu.RUnlock()
	}
	if generationAware && identity == nil {
		return nil, nil
	}
	if h.vectorCache == nil && h.cache == nil {
		return nil, nil
	}

	var queryVecs [][]float32
	var err error
	if generationAware {
		queryVecs, err = EmbedForPurpose(ctx, h.embedder, EmbeddingPurposeQuery, []string{query})
	} else {
		queryVecs, err = h.embedder.Embed(ctx, []string{query})
	}
	if err != nil {
		return nil, err
	}
	if generationAware && len(queryVecs) != 1 {
		return nil, fmt.Errorf("embedding provider returned %d query vectors, want 1", len(queryVecs))
	}
	if len(queryVecs) == 0 || len(queryVecs[0]) == 0 {
		return nil, nil
	}

	queryVec := queryVecs[0]
	if generationAware && len(queryVec) != identity.Dimensions {
		return nil, fmt.Errorf("%w: query has %d, want %d",
			ErrEmbeddingDimensionMismatch, len(queryVec), identity.Dimensions)
	}
	queryNorm := VectorNorm(queryVec)

	var results []SearchResult

	if h.vectorCache != nil && h.vectorCache.Len() > 0 {
		// Single allocation-light pass over the cache (no map copy, no
		// re-quantization): the quantized form + norm are memoized per vector.
		scored := h.vectorCache.Score(queryVec, queryNorm, 0.01, maxResults)

		for _, s := range scored {
			m, err := h.store.Get(ctx, s.ID)
			if err != nil || m == nil {
				continue
			}
			if !matchesSearchOptions(*m, opts) {
				continue
			}
			score := s.Score
			if len(queryEntities) > 0 && containsEntity(m.Content, m.Keywords, queryEntities) {
				score *= 1.1
			}
			results = append(results, SearchResult{Memory: *m, Score: score})
		}
	} else if !generationAware && h.cache != nil {
		memories, err := h.store.List(ctx, ListOptions{Limit: 10000, Category: opts.Category, ProjectID: opts.ProjectID})
		if err != nil {
			return nil, err
		}

		for _, m := range memories {
			text := m.Content
			if len(m.Keywords) > 0 {
				text += " " + strings.Join(m.Keywords, " ")
			}

			cached, ok := h.cache.Get(ctx, text, h.embedder.Model())
			if !ok {
				continue
			}
			if len(cached) == 0 {
				continue
			}

			qe := QuantizeFloat32(cached)
			score := qe.CosineSimilarity(queryVec, queryNorm)
			if score > 0.01 {
				if len(queryEntities) > 0 && containsEntity(m.Content, m.Keywords, queryEntities) {
					score *= 1.1
				}
				results = append(results, SearchResult{Memory: m, Score: score})
			}
		}

		sort.Slice(results, func(i, j int) bool {
			return results[i].Score > results[j].Score
		})
		if len(results) > maxResults {
			results = results[:maxResults]
		}
	}

	return results, nil
}

func (h *HybridSearcher) searchBM25(ctx context.Context, query string, maxResults int, queryEntities []string, opts SearchOptions) ([]SearchResult, error) {
	// Prefer the advanced expansion (synonyms, accent-folding, NEAR/phrase
	// handling). Fall back to the simple keyword expansion when it yields
	// nothing (e.g. query is only stopwords).
	ftsQuery := ExpandQueryAdvanced(query)
	if ftsQuery == "" {
		keywords := ExtractKeywords(query)
		if len(keywords) == 0 {
			return nil, nil
		}
		ftsQuery = ExpandQueryForFTS(keywords)
	}
	if ftsQuery == "" {
		return nil, nil
	}

	results, err := h.store.Search(ctx, ftsQuery, SearchOptions{MaxResults: maxResults, Category: opts.Category, ProjectID: opts.ProjectID})
	if err != nil {
		return nil, err
	}

	if len(queryEntities) > 0 {
		for i := range results {
			if containsEntity(results[i].Memory.Content, results[i].Memory.Keywords, queryEntities) {
				results[i].Score *= 1.1
			}
		}
	}

	return results, nil
}

func matchesSearchOptions(m Memory, opts SearchOptions) bool {
	if opts.Category != "" && m.Category != opts.Category {
		return false
	}
	if opts.ProjectID != "" {
		if m.ProjectID == nil || *m.ProjectID != opts.ProjectID {
			return false
		}
	}
	return true
}

// fuse combines the vector and BM25 result lists into a single ranking.
// Unlike pure rank-based RRF (which throws away the actual relevance and is
// dominated by whichever signal ranked an item #1), it preserves the magnitude
// of each signal: scores within each list are normalized to [0,1] by that
// list's max, then combined as a weighted sum. A 0.95-cosine hit therefore
// stays clearly ahead of a 0.55 one, and MinScore becomes a stable relevance
// cutoff (fraction of the best hit) instead of being coupled to list length.
func (h *HybridSearcher) fuse(vecResults, bm25Results []SearchResult, vectorWeight, bm25Weight float64) []SearchResult {
	type scored struct {
		memory Memory
		score  float64
	}

	scoreMap := make(map[string]*scored)

	merge := func(results []SearchResult, weight float64) {
		var max float64
		for _, r := range results {
			if r.Score > max {
				max = r.Score
			}
		}
		if max <= 0 {
			max = 1
		}
		for _, r := range results {
			contrib := weight * (r.Score / max)
			if existing, ok := scoreMap[r.Memory.ID]; ok {
				existing.score += contrib
			} else {
				scoreMap[r.Memory.ID] = &scored{memory: r.Memory, score: contrib}
			}
		}
	}

	merge(vecResults, vectorWeight)
	merge(bm25Results, bm25Weight)

	minScore := h.config.MinScore
	if minScore <= 0 {
		minScore = 0.05
	}

	var merged []SearchResult
	for _, s := range scoreMap {
		if s.score >= minScore {
			merged = append(merged, SearchResult{Memory: s.memory, Score: s.score})
		}
	}

	return merged
}

func (h *HybridSearcher) applyProjectBoost(results []SearchResult, projectID string) []SearchResult {
	const (
		boostActive = 1.3
		boostGlobal = 1.1
	)
	for i := range results {
		pid := results[i].Memory.ProjectID
		if pid != nil && *pid == projectID {
			results[i].Score *= boostActive
		} else if pid == nil || *pid == "" {
			results[i].Score *= boostGlobal
		}
	}
	return results
}

// workingSetBoost is the multiplier applied to a result whose content or
// keywords overlap the session's current focus. Matches applyProjectBoost's
// active-project boost so a focused, on-topic memory ranks comparably to one in
// the active project.
const workingSetBoost = 1.3

// applyWorkingSetBoost multiplies the score of results that mention any token
// from the working set (file basenames, symbols, entities) in their content or
// keywords. Token matching is case-insensitive and substring-based; tokens
// shorter than 3 runes are ignored to avoid spurious matches. When explain is
// set, a "working_set" signal is appended to each boosted result.
func applyWorkingSetBoost(results []SearchResult, ws *WorkingSetSignals, explain bool) []SearchResult {
	tokens := workingSetTokens(ws)
	if len(tokens) == 0 {
		return results
	}
	for i := range results {
		hay := strings.ToLower(results[i].Memory.Content + " " + strings.Join(results[i].Memory.Keywords, " "))
		for _, tok := range tokens {
			if strings.Contains(hay, tok) {
				results[i].Score *= workingSetBoost
				if explain {
					results[i].Signals = appendSignal(results[i].Signals, "working_set")
				}
				break
			}
		}
		if results[i].Score > 10.0 {
			results[i].Score = 10.0
		}
	}
	return results
}

// workingSetTokens flattens the working set into a lower-cased, deduped token
// list. File paths contribute their basename (and basename without extension)
// so a memory mentioning "client.go" matches a working-set entry of
// "pkg/sync/client.go".
func workingSetTokens(ws *WorkingSetSignals) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if len([]rune(s)) < 3 || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, f := range ws.Files {
		base := f
		if idx := strings.LastIndexAny(base, "/\\"); idx >= 0 {
			base = base[idx+1:]
		}
		add(base) // full basename incl. extension, e.g. "client.go"
		// The extension-stripped stem is only added when it's 4+ runes: short
		// stems like "run" (from run.go) would substring-match unrelated words
		// ("running", "runtime"); "client.go" itself is specific enough.
		if dot := strings.LastIndex(base, "."); dot > 0 {
			if stem := base[:dot]; len([]rune(stem)) >= 4 {
				add(stem)
			}
		}
	}
	for _, s := range ws.Symbols {
		add(s)
	}
	for _, e := range ws.Entities {
		add(e)
	}
	return out
}

// annotateBaseSignals appends ranking-rationale signals derived from each
// result's project membership and lifecycle metadata. Called only in explain
// mode; it does not alter scores.
func annotateBaseSignals(results []SearchResult, boostProjectID string, now time.Time) {
	for i := range results {
		pid := results[i].Memory.ProjectID
		switch {
		case boostProjectID != "" && pid != nil && *pid == boostProjectID:
			results[i].Signals = appendSignal(results[i].Signals, "project")
		case pid == nil || *pid == "":
			results[i].Signals = appendSignal(results[i].Signals, "global")
		}
		meta := ParseMetadata(results[i].Memory.Metadata)
		if meta.Pinned {
			results[i].Signals = appendSignal(results[i].Signals, "pinned")
		}
		if meta.CurationStatus == CurationStatusLowSignal {
			results[i].Signals = appendSignal(results[i].Signals, "low_signal")
		}
		if now.Sub(results[i].Memory.CreatedAt) <= 7*24*time.Hour {
			results[i].Signals = appendSignal(results[i].Signals, "fresh")
		}
		if !meta.Pinned && !results[i].Memory.CreatedAt.IsZero() {
			last := results[i].Memory.CreatedAt
			if meta.LastUsedAt != "" {
				if t, err := time.Parse(time.RFC3339, meta.LastUsedAt); err == nil && t.After(last) {
					last = t
				}
			}
			if now.Sub(last) > decayAgingAge {
				results[i].Signals = appendSignal(results[i].Signals, "decayed")
			}
		}
	}
}

// Age-decay bands (Feature E). Generous on purpose: decay is a gentle nudge
// for never-reinforced memories, not a hard demotion — low_signal handles
// those.
const (
	decayAgingAge    = 90 * 24 * time.Hour
	decayAgingFactor = 0.85
	decayStaleAge    = 180 * 24 * time.Hour
	decayStaleFactor = 0.7
)

func appendSignal(sigs []string, s string) []string {
	for _, existing := range sigs {
		if existing == s {
			return sigs
		}
	}
	return append(sigs, s)
}

func applyLifecycleBoost(results []SearchResult, now time.Time) []SearchResult {
	for i := range results {
		meta := ParseMetadata(results[i].Memory.Metadata)

		if meta.Pinned {
			results[i].Score *= 1.5
		}

		if meta.Importance > 0 {
			results[i].Score *= 1.0 + meta.Importance*0.3
		}

		// Demotion for weak memories. The two conditions are mutually
		// exclusive: low_signal is the explicit flag and wins; the
		// quality-score band is a softer fallback for memories scored below
		// threshold that were never flagged. Multiplying both stacked to
		// ~0.0045 and effectively erased legitimate hits.
		demoted := false
		switch {
		case meta.CurationStatus == CurationStatusLowSignal:
			results[i].Score *= 0.03
			demoted = true
		case meta.QualityScore > 0 && meta.QualityScore < RemoteQualityThreshold && !meta.Pinned:
			results[i].Score *= 0.15
			demoted = true
		}

		// Age decay (Feature E): memories that were never drawn on fade
		// gently with age instead of competing forever at full strength.
		// Computed at search time — nothing is written back, so the decay is
		// trivially reversible and never violates the "importance is only
		// initialized, never reduced" rule. Reinforcement (any recorded use)
		// resets the clock; pinned memories never decay, and decay never
		// stacks on an already-demoted memory (the 0.5.8 lesson: stacked
		// multipliers erase legitimate hits).
		// A zero CreatedAt means "unknown", not "ancient" — never decay on it.
		if !meta.Pinned && !demoted && !results[i].Memory.CreatedAt.IsZero() {
			last := results[i].Memory.CreatedAt
			if meta.LastUsedAt != "" {
				if t, err := time.Parse(time.RFC3339, meta.LastUsedAt); err == nil && t.After(last) {
					last = t
				}
			}
			switch age := now.Sub(last); {
			case age > decayStaleAge:
				results[i].Score *= decayStaleFactor
			case age > decayAgingAge:
				results[i].Score *= decayAgingFactor
			}
		}

		switch meta.Kind {
		case "decision", "learning", "rule":
			results[i].Score *= 1.15
		case "handoff":
			if !meta.IsExpired(now) {
				results[i].Score *= 1.2
			} else {
				results[i].Score *= 0.5
			}
		case "precompact":
			if !meta.IsExpired(now) {
				results[i].Score *= 1.1
			} else {
				results[i].Score *= 0.3
			}
		}

		if meta.IsSemantic() {
			results[i].Score *= 1.1
		}
		if meta.IsOperational() && meta.IsExpired(now) {
			results[i].Score *= 0.3
		}
		if meta.IsOperational() && !meta.IsExpired(now) {
			results[i].Score *= 1.05
		}

		if len(meta.Supersedes) > 0 {
			results[i].Score *= 0.7
		}

		if meta.Origin == "bootstrap" && meta.Confidence < 0.5 {
			results[i].Score *= 0.8
		}

		if meta.ContextTier == "L0" {
			results[i].Score *= 1.3
		} else if meta.ContextTier == "L1" {
			results[i].Score *= 1.15
		}

		// Clamp: multiplicative boosts can stack excessively.
		if results[i].Score > 10.0 {
			results[i].Score = 10.0
		}
	}
	return results
}

// categoryDecayMultiplier scales the half-life by category so durable knowledge
// barely decays while time-bound entries decay at the base rate. Without this,
// a 60-day-old "decision" would lose ~75% of its score at a 30-day half-life
// and get buried under fresh noise.
func categoryDecayMultiplier(category string) float64 {
	switch category {
	case "fact", "decision", "preference":
		return 6 // durable: ~6x the base half-life
	case "learning", "summary":
		return 3
	default: // event, plan, or uncategorized: decay at the base rate
		return 1
	}
}

func (h *HybridSearcher) applyTemporalDecay(results []SearchResult, cfg HybridSearchConfig) []SearchResult {
	if !cfg.TemporalDecayEnabled || len(results) == 0 {
		return results
	}

	baseHalfLife := float64(cfg.TemporalDecayHalfLifeDays)
	if baseHalfLife <= 0 {
		baseHalfLife = 30
	}
	now := time.Now()

	for i := range results {
		halfLife := baseHalfLife * categoryDecayMultiplier(results[i].Memory.Category)
		lambda := math.Log(2) / halfLife
		ageDays := now.Sub(results[i].Memory.CreatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		results[i].Score *= math.Exp(-lambda * ageDays)
	}

	return results
}

// applyMMR re-selects results for relevance + diversity (Maximal Marginal
// Relevance) and drops near-identical hits. Similarity uses embedding cosine
// when both vectors are cached (semantic dedup — catches paraphrases that share
// no tokens) and falls back to lexical Jaccard otherwise. It runs even when the
// result set is small so duplicates are removed regardless of count.
func (h *HybridSearcher) applyMMR(results []SearchResult, lambda float64, maxResults int) []SearchResult {
	if len(results) <= 1 {
		return results
	}

	if lambda <= 0 {
		lambda = 0.7
	}
	if lambda > 1 {
		lambda = 1
	}
	const nearDupThreshold = 0.97 // drop a candidate that is ~identical to one already selected

	limit := maxResults
	if limit <= 0 || limit > len(results) {
		limit = len(results)
	}

	tokenCache := make(map[string]map[string]bool)
	tokenize := func(text string) map[string]bool {
		if cached, ok := tokenCache[text]; ok {
			return cached
		}
		tokens := make(map[string]bool)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			if len(word) > 2 {
				tokens[word] = true
			}
		}
		tokenCache[text] = tokens
		return tokens
	}
	sim := func(a, b SearchResult) float64 {
		if h.vectorCache != nil {
			va, oka := h.vectorCache.Get(a.Memory.ID)
			vb, okb := h.vectorCache.Get(b.Memory.ID)
			if oka && okb && len(va) > 0 && len(vb) > 0 {
				return cosineSimilarityFloat32(va, vb)
			}
		}
		return jaccardSimilarity(tokenize(a.Memory.Content), tokenize(b.Memory.Content))
	}

	selected := make([]SearchResult, 0, limit)
	remaining := make([]SearchResult, len(results))
	copy(remaining, results)

	selected = append(selected, remaining[0])
	remaining = remaining[1:]

	for len(selected) < limit && len(remaining) > 0 {
		bestIdx := -1
		bestScore := -math.MaxFloat64

		for i, candidate := range remaining {
			maxSim := 0.0
			for _, sel := range selected {
				if s := sim(candidate, sel); s > maxSim {
					maxSim = s
				}
			}
			if maxSim >= nearDupThreshold {
				continue // near-duplicate of an already-selected result — skip
			}
			mmrScore := lambda*candidate.Score - (1-lambda)*maxSim
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = i
			}
		}

		if bestIdx < 0 {
			break // everything remaining is a near-duplicate
		}
		selected = append(selected, remaining[bestIdx])
		remaining = append(remaining[:bestIdx], remaining[bestIdx+1:]...)
	}

	return selected
}

func jaccardSimilarity(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	if len(a) == 0 || len(b) == 0 {
		return 0
	}

	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func containsEntity(content string, keywords []string, entities []string) bool {
	contentLower := strings.ToLower(content)
	for _, e := range entities {
		eLower := strings.ToLower(e)
		if strings.Contains(contentLower, eLower) {
			return true
		}
	}
	for _, kw := range keywords {
		kwLower := strings.ToLower(kw)
		for _, e := range entities {
			if strings.ToLower(e) == kwLower {
				return true
			}
		}
	}
	return false
}
