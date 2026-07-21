package eval

import (
	"context"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

type evalEmbeddingProvider struct{}

func (evalEmbeddingProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		switch text {
		case "semantic-query":
			result[i] = []float32{1, 0}
		default:
			result[i] = []float32{0, 1}
		}
	}
	return result, nil
}
func (evalEmbeddingProvider) Dimensions() int { return 2 }
func (evalEmbeddingProvider) Name() string    { return "eval" }
func (evalEmbeddingProvider) Model() string   { return "eval-v1" }
func (evalEmbeddingProvider) Close() error    { return nil }

type actualRetrievalEngine struct {
	store    *memory.SQLiteStore
	embedder evalEmbeddingProvider
	project  string
	logger   *slog.Logger
}

func (e *actualRetrievalEngine) SearchStreams(
	ctx context.Context,
	query string,
	streams []RetrievalStream,
	k int,
) ([]string, error) {
	if len(streams) == 1 {
		switch streams[0] {
		case StreamBM25:
			results, err := e.store.Search(ctx, memory.ExpandQueryAdvanced(query),
				memory.SearchOptions{ProjectID: e.project, MaxResults: k})
			return resultIDs(results), err
		case StreamVector:
			vectors, err := e.embedder.Embed(ctx, []string{query})
			if err != nil {
				return nil, err
			}
			norm := 0.0
			for _, value := range vectors[0] {
				norm += float64(value * value)
			}
			scored := e.store.VectorCache().Score(vectors[0], math.Sqrt(norm), -1, k)
			ids := make([]string, 0, len(scored))
			for _, item := range scored {
				ids = append(ids, item.ID)
			}
			return ids, nil
		}
	}
	config := memory.DefaultHybridSearchConfig()
	config.MaxResults = k
	config.MMREnabled = false
	config.TemporalDecayEnabled = false
	searcher := memory.NewHybridSearcher(
		e.store, e.embedder, nil, e.store.VectorCache(), config, nil, nil, e.logger,
	)
	results, err := searcher.Search(ctx, query, memory.SearchOptions{
		ProjectID: e.project, MaxResults: k,
	})
	return resultIDs(results), err
}

func resultIDs(results []memory.SearchResult) []string {
	ids := make([]string, 0, len(results))
	for _, result := range results {
		ids = append(ids, result.Memory.ID)
	}
	return ids
}

func TestActualRetrievalPipelineAblationsAndVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "pipeline-eval.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project := "eval-project"
	corpus := []memory.Memory{
		{ID: "lexical", ProjectID: &project, Category: "fact", Content: "lexical-query exact token", Source: "eval"},
		{ID: "semantic", ProjectID: &project, Category: "fact", Content: "opaque semantic source", Source: "eval"},
		{ID: "mixed", ProjectID: &project, Category: "decision", Content: "mixed-query exact decision", Source: "eval"},
	}
	start := time.Now().UTC()
	for _, item := range corpus {
		if err := store.Save(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	canonicalAt := time.Now().UTC()
	store.VectorCache().Put("semantic", []float32{1, 0})
	store.VectorCache().Put("mixed", []float32{0, 1})
	vectorAt := time.Now().UTC()
	engine := &actualRetrievalEngine{
		store: store, project: project, embedder: evalEmbeddingProvider{},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	identity, err := memory.EmbeddingIdentityOf(engine.embedder)
	if err != nil {
		t.Fatal(err)
	}
	baseMetadata := RetrievalRunMetadata{
		CorpusSize: len(corpus), FTS5Available: true,
		EmbeddingModel: identity.Model, EmbeddingDims: identity.Dimensions,
		SemanticSpaceID: identity.SemanticSpaceID(),
		Exclusions:      []string{"remote visibility requires an integration endpoint"},
	}
	runs := []struct {
		name    string
		streams []RetrievalStream
		query   RankedQuery
	}{
		{"bm25", []RetrievalStream{StreamBM25}, RankedQuery{Name: "lexical", Query: "lexical-query", Expected: []string{"lexical"}}},
		{"vector", []RetrievalStream{StreamVector}, RankedQuery{Name: "semantic", Query: "semantic-query", Expected: []string{"semantic"}}},
		{"fused", []RetrievalStream{StreamBM25, StreamVector}, RankedQuery{Name: "mixed", Query: "mixed-query", Expected: []string{"mixed"}}},
	}
	for _, run := range runs {
		report, err := RunRetrievalAblation(
			ctx, run.name, engine, []RankedQuery{run.query}, run.streams, 3, baseMetadata,
		)
		if err != nil {
			t.Fatalf("%s: %v", run.name, err)
		}
		if report.Mean.Recall != 1 || report.Mean.MRR != 1 {
			t.Fatalf("%s metrics=%+v", run.name, report.Mean)
		}
	}
	milestones := VisibilityMilestones{
		CanonicalAt: canonicalAt, VectorAt: &vectorAt,
	}
	if err := milestones.Validate(); err != nil {
		t.Fatal(err)
	}
	latencies := milestones.Latencies()
	if latencies["canonical"] != 0 || latencies["vector"] < 0 ||
		canonicalAt.Before(start) {
		t.Fatalf("visibility latencies=%v", latencies)
	}
}
