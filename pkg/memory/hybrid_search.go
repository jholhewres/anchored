package memory

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"
)

type HybridSearchConfig struct {
	VectorWeight         float64
	BM25Weight           float64
	MaxResults           int
	MinScore             float64
	MMREnabled           bool
	MMRLambda            float64
	TemporalDecayEnabled bool
	TemporalDecayHalfLifeDays int
}

func DefaultHybridSearchConfig() HybridSearchConfig {
	return HybridSearchConfig{
		VectorWeight:         0.7,
		BM25Weight:           0.3,
		MaxResults:           20,
		MinScore:             0.05,
		MMREnabled:           true,
		MMRLambda:            0.7,
		TemporalDecayEnabled: true,
		TemporalDecayHalfLifeDays: 30,
	}
}

type HybridSearcher struct {
	store   Store
	embedder EmbeddingProvider
	cache   *EmbeddingCache
	config  HybridSearchConfig
	logger  *slog.Logger
}

func NewHybridSearcher(store Store, embedder EmbeddingProvider, cache *EmbeddingCache, cfg HybridSearchConfig, logger *slog.Logger) *HybridSearcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &HybridSearcher{store: store, embedder: embedder, cache: cache, config: cfg, logger: logger}
}

func (h *HybridSearcher) Search(ctx context.Context, query string) ([]SearchResult, error) {
	cfg := h.config
	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = 20
	}

	vecResults, vecErr := h.searchVector(ctx, query, maxResults*4)
	bm25Results, bm25Err := h.searchBM25(ctx, query, maxResults*4)

	if vecErr != nil {
		h.logger.Warn("vector search failed, using BM25 only", "error", vecErr)
	}
	if bm25Err != nil {
		h.logger.Warn("BM25 search failed, using vector only", "error", bm25Err)
	}

	fused := h.rrfFuse(vecResults, bm25Results, cfg.VectorWeight, cfg.BM25Weight)

	fused = h.applyTemporalDecay(fused, cfg)

	fused = h.applyMMR(fused, cfg.MMRLambda, maxResults)

	sort.Slice(fused, func(i, j int) bool {
		return fused[i].Score > fused[j].Score
	})

	if len(fused) > maxResults {
		fused = fused[:maxResults]
	}

	return fused, nil
}

func (h *HybridSearcher) searchVector(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	if h.embedder == nil || h.cache == nil {
		return nil, nil
	}

	queryVecs, err := h.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(queryVecs) == 0 || len(queryVecs[0]) == 0 {
		return nil, nil
	}

	queryVec := queryVecs[0]
	queryNorm := VectorNorm(queryVec)

	memories, err := h.store.List(ctx, ListOptions{Limit: 10000})
	if err != nil {
		return nil, err
	}

	var results []SearchResult
	for _, m := range memories {
		text := m.Content
		if m.Keywords != nil && len(m.Keywords) > 0 {
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
			results = append(results, SearchResult{Memory: m, Score: score})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > maxResults {
		results = results[:maxResults]
	}

	return results, nil
}

func (h *HybridSearcher) searchBM25(ctx context.Context, query string, maxResults int) ([]SearchResult, error) {
	keywords := ExtractKeywords(query)
	if len(keywords) == 0 {
		return nil, nil
	}

	ftsQuery := ExpandQueryForFTS(keywords)
	if ftsQuery == "" {
		return nil, nil
	}

	results, err := h.store.Search(ctx, ftsQuery, SearchOptions{MaxResults: maxResults})
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (h *HybridSearcher) rrfFuse(vecResults, bm25Results []SearchResult, vectorWeight, bm25Weight float64) []SearchResult {
	type scored struct {
		memory Memory
		score  float64
	}

	scoreMap := make(map[string]*scored)

	merge := func(results []SearchResult, weight float64) {
		for i, r := range results {
			key := r.Memory.ID
			if existing, ok := scoreMap[key]; ok {
				existing.score += weight * (1.0 / float64(i+1))
			} else {
				scoreMap[key] = &scored{memory: r.Memory, score: weight * (1.0 / float64(i+1))}
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

func (h *HybridSearcher) applyTemporalDecay(results []SearchResult, cfg HybridSearchConfig) []SearchResult {
	if !cfg.TemporalDecayEnabled || len(results) == 0 {
		return results
	}

	halfLife := float64(cfg.TemporalDecayHalfLifeDays)
	if halfLife <= 0 {
		halfLife = 30
	}
	lambda := math.Log(2) / halfLife
	now := time.Now()

	for i := range results {
		ageDays := now.Sub(results[i].Memory.CreatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		results[i].Score *= math.Exp(-lambda * ageDays)
	}

	return results
}

func (h *HybridSearcher) applyMMR(results []SearchResult, lambda float64, maxResults int) []SearchResult {
	if len(results) <= maxResults {
		return results
	}

	if lambda <= 0 {
		lambda = 0.7
	}
	if lambda > 1 {
		lambda = 1
	}

	selected := make([]SearchResult, 0, maxResults)
	remaining := make([]SearchResult, len(results))
	copy(remaining, results)

	selected = append(selected, remaining[0])
	remaining = remaining[1:]

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

	for len(selected) < maxResults && len(remaining) > 0 {
		bestIdx := 0
		bestScore := -1.0

		for i, candidate := range remaining {
			maxSim := 0.0
			candidateTokens := tokenize(candidate.Memory.Content)
			for _, sel := range selected {
				sim := jaccardSimilarity(candidateTokens, tokenize(sel.Memory.Content))
				if sim > maxSim {
					maxSim = sim
				}
			}

			mmrScore := lambda*candidate.Score - (1-lambda)*maxSim
			if mmrScore > bestScore {
				bestScore = mmrScore
				bestIdx = i
			}
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
