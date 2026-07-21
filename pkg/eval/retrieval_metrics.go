package eval

import (
	"context"
	"fmt"
	"sort"
	"time"
)

type RetrievalStream string

const (
	StreamBM25       RetrievalStream = "bm25"
	StreamVector     RetrievalStream = "vector"
	StreamKG         RetrievalStream = "kg"
	StreamFederation RetrievalStream = "federation"
)

type RankedQuery struct {
	Name     string
	Query    string
	Expected []string
}

type RetrievalEngine interface {
	SearchStreams(ctx context.Context, query string, streams []RetrievalStream, k int) ([]string, error)
}

type RetrievalRunMetadata struct {
	CorpusSize      int               `json:"corpus_size"`
	Streams         []RetrievalStream `json:"streams"`
	EmbeddingModel  string            `json:"embedding_model,omitempty"`
	EmbeddingDims   int               `json:"embedding_dimensions,omitempty"`
	SemanticSpaceID string            `json:"semantic_space_id,omitempty"`
	FTS5Available   bool              `json:"fts5_available"`
	Exclusions      []string          `json:"exclusions,omitempty"`
}

type RankingScore struct {
	Recall float64 `json:"recall"`
	MRR    float64 `json:"mrr"`
	NDCG   float64 `json:"ndcg"`
}

type AblationResult struct {
	Name     string                  `json:"name"`
	K        int                     `json:"k"`
	Streams  []RetrievalStream       `json:"streams"`
	Mean     RankingScore            `json:"mean"`
	Queries  map[string]RankingScore `json:"queries"`
	Metadata RetrievalRunMetadata    `json:"metadata"`
}

// RunRetrievalAblation evaluates one explicit stream combination. Calling it
// repeatedly with BM25-only, vector-only, KG-only, and fused combinations
// produces directly comparable reports without coupling evals to a concrete
// search implementation.
func RunRetrievalAblation(
	ctx context.Context,
	name string,
	engine RetrievalEngine,
	queries []RankedQuery,
	streams []RetrievalStream,
	k int,
	metadata RetrievalRunMetadata,
) (AblationResult, error) {
	if engine == nil {
		return AblationResult{}, fmt.Errorf("retrieval eval: engine is required")
	}
	if k <= 0 {
		k = 10
	}
	for _, stream := range streams {
		if !validRetrievalStream(stream) {
			return AblationResult{}, fmt.Errorf("retrieval eval: unknown stream %q", stream)
		}
	}
	streams = canonicalStreams(streams)
	metadata.Streams = append([]RetrievalStream(nil), streams...)
	if err := metadata.Validate(); err != nil {
		return AblationResult{}, err
	}
	result := AblationResult{
		Name:     name,
		K:        k,
		Streams:  append([]RetrievalStream(nil), streams...),
		Queries:  make(map[string]RankingScore, len(queries)),
		Metadata: metadata,
	}
	for _, query := range queries {
		if query.Name == "" {
			return AblationResult{}, fmt.Errorf("retrieval eval: query name is required")
		}
		if _, duplicate := result.Queries[query.Name]; duplicate {
			return AblationResult{}, fmt.Errorf("retrieval eval: duplicate query name %q", query.Name)
		}
		ranked, err := engine.SearchStreams(ctx, query.Query, streams, k)
		if err != nil {
			return AblationResult{}, fmt.Errorf("retrieval eval %q: %w", query.Name, err)
		}
		score := ScoreRanking(query.Expected, ranked, k)
		result.Queries[query.Name] = score
		result.Mean.Recall += score.Recall
		result.Mean.MRR += score.MRR
		result.Mean.NDCG += score.NDCG
	}
	if len(queries) > 0 {
		n := float64(len(queries))
		result.Mean.Recall /= n
		result.Mean.MRR /= n
		result.Mean.NDCG /= n
	}
	return result, nil
}

func ScoreRanking(expected, ranked []string, k int) RankingScore {
	if k <= 0 {
		k = len(ranked)
	}
	want := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, k)
	relevance := make([]bool, 0, k)
	hits := 0
	for _, id := range ranked {
		if len(relevance) >= k {
			break
		}
		if _, duplicate := seen[id]; duplicate {
			// Duplicate results consume a real ranking position but cannot
			// count as another relevant document.
			relevance = append(relevance, false)
			continue
		}
		seen[id] = struct{}{}
		_, relevant := want[id]
		relevance = append(relevance, relevant)
		if relevant {
			hits++
		}
	}
	recall := 1.0
	if len(want) > 0 {
		recall = float64(hits) / float64(len(want))
	}
	return RankingScore{
		Recall: recall,
		MRR:    reciprocalRank(relevance),
		NDCG:   normalizedDiscountedCumulativeGain(relevance, len(want), k),
	}
}

func (m RetrievalRunMetadata) Validate() error {
	if m.CorpusSize <= 0 {
		return fmt.Errorf("retrieval eval: corpus size must be positive")
	}
	if len(m.Streams) == 0 {
		return fmt.Errorf("retrieval eval: at least one stream is required")
	}
	for _, stream := range m.Streams {
		if stream == StreamVector {
			if m.EmbeddingModel == "" || m.EmbeddingDims <= 0 || m.SemanticSpaceID == "" {
				return fmt.Errorf("retrieval eval: vector runs require model, dimensions, and semantic space")
			}
		}
	}
	return nil
}

func canonicalStreams(streams []RetrievalStream) []RetrievalStream {
	seen := make(map[RetrievalStream]struct{}, len(streams))
	out := make([]RetrievalStream, 0, len(streams))
	for _, stream := range streams {
		if _, ok := seen[stream]; ok {
			continue
		}
		seen[stream] = struct{}{}
		out = append(out, stream)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func validRetrievalStream(stream RetrievalStream) bool {
	switch stream {
	case StreamBM25, StreamVector, StreamKG, StreamFederation:
		return true
	default:
		return false
	}
}

type TemporalCandidate struct {
	ID         string
	ValidFrom  time.Time
	ValidTo    *time.Time
	SystemFrom time.Time
	SystemTo   *time.Time
}

// TemporalLeakage returns candidates that were not valid and known at the
// evaluation axes. Empty means the historical retrieval contained no future
// information.
func TemporalLeakage(candidates []TemporalCandidate, validAt, knownAt time.Time) []string {
	var leaked []string
	for _, candidate := range candidates {
		valid := !validAt.Before(candidate.ValidFrom) &&
			(candidate.ValidTo == nil || validAt.Before(*candidate.ValidTo))
		known := !knownAt.Before(candidate.SystemFrom) &&
			(candidate.SystemTo == nil || knownAt.Before(*candidate.SystemTo))
		if !valid || !known {
			leaked = append(leaked, candidate.ID)
		}
	}
	sort.Strings(leaked)
	return leaked
}

type VisibilityMilestones struct {
	CanonicalAt time.Time  `json:"canonical_at"`
	VectorAt    *time.Time `json:"vector_at,omitempty"`
	KGAt        *time.Time `json:"kg_at,omitempty"`
	RemoteAt    *time.Time `json:"remote_at,omitempty"`
}

func (m VisibilityMilestones) Latencies() map[string]time.Duration {
	out := make(map[string]time.Duration)
	if m.CanonicalAt.IsZero() {
		return out
	}
	out["canonical"] = 0
	for name, at := range map[string]*time.Time{
		"vector": m.VectorAt,
		"kg":     m.KGAt,
		"remote": m.RemoteAt,
	} {
		if at != nil && !at.Before(m.CanonicalAt) {
			out[name] = at.Sub(m.CanonicalAt)
		}
	}
	return out
}

func (m VisibilityMilestones) Validate() error {
	if m.CanonicalAt.IsZero() {
		return fmt.Errorf("visibility milestones: canonical time is required")
	}
	for name, at := range map[string]*time.Time{
		"vector": m.VectorAt, "kg": m.KGAt, "remote": m.RemoteAt,
	} {
		if at != nil && at.Before(m.CanonicalAt) {
			return fmt.Errorf("visibility milestones: %s precedes canonical", name)
		}
	}
	return nil
}
