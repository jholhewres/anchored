package memory

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"
)

func projectIDPtr(s string) *string { return &s }

func TestApplyProjectBoost_ActiveProject(t *testing.T) {
	proj1 := "proj-1"
	pid := projectIDPtr(proj1)
	results := []SearchResult{
		{Memory: Memory{ID: "1", ProjectID: pid, CreatedAt: time.Now()}, Score: 1.0},
		{Memory: Memory{ID: "2", ProjectID: nil, CreatedAt: time.Now()}, Score: 1.0},
		{Memory: Memory{ID: "3", ProjectID: projectIDPtr("proj-2"), CreatedAt: time.Now()}, Score: 1.0},
	}

	h := &HybridSearcher{}
	boosted := h.applyProjectBoost(results, proj1)

	if boosted[0].Score != 1.3 {
		t.Errorf("active project boost: got %f, want 1.3", boosted[0].Score)
	}
	if boosted[1].Score != 1.1 {
		t.Errorf("global boost: got %f, want 1.1", boosted[1].Score)
	}
	if boosted[2].Score != 1.0 {
		t.Errorf("other project: got %f, want 1.0", boosted[2].Score)
	}
}

func TestApplyProjectBoost_EmptyProjectID(t *testing.T) {
	pid := projectIDPtr("proj-1")
	empty := ""
	results := []SearchResult{
		{Memory: Memory{ID: "1", ProjectID: pid, CreatedAt: time.Now()}, Score: 1.0},
		{Memory: Memory{ID: "2", ProjectID: nil, CreatedAt: time.Now()}, Score: 1.0},
		{Memory: Memory{ID: "3", ProjectID: &empty, CreatedAt: time.Now()}, Score: 1.0},
	}

	h := &HybridSearcher{config: DefaultHybridSearchConfig()}
	boosted := h.applyProjectBoost(results, "proj-1")

	if boosted[0].Score != 1.3 {
		t.Errorf("got %f, want 1.3", boosted[0].Score)
	}
	if boosted[1].Score != 1.1 {
		t.Errorf("got %f, want 1.1", boosted[1].Score)
	}
	if boosted[2].Score != 1.1 {
		t.Errorf("got %f, want 1.1", boosted[2].Score)
	}
}

func TestApplyProjectBoost_ReorderAfterBoost(t *testing.T) {
	proj1 := "proj-1"
	pid := projectIDPtr(proj1)
	other := projectIDPtr("proj-2")

	// Lower base score but should win after ×1.3 boost
	results := []SearchResult{
		{Memory: Memory{ID: "other", ProjectID: other, CreatedAt: time.Now()}, Score: 1.0},
		{Memory: Memory{ID: "active", ProjectID: pid, CreatedAt: time.Now()}, Score: 0.9},
		{Memory: Memory{ID: "global", ProjectID: nil, CreatedAt: time.Now()}, Score: 0.8},
	}

	h := &HybridSearcher{config: DefaultHybridSearchConfig()}
	boosted := h.applyProjectBoost(results, proj1)

	sort.Slice(boosted, func(i, j int) bool {
		return boosted[i].Score > boosted[j].Score
	})

	// 0.9*1.3=1.17 > 1.0*1.0=1.0 > 0.8*1.1=0.88
	if boosted[0].Memory.ID != "active" {
		t.Errorf("first should be 'active', got %s", boosted[0].Memory.ID)
	}
	if boosted[1].Memory.ID != "other" {
		t.Errorf("second should be 'other', got %s", boosted[1].Memory.ID)
	}
	if boosted[2].Memory.ID != "global" {
		t.Errorf("third should be 'global', got %s", boosted[2].Memory.ID)
	}
}

type hybridMockStore struct {
	memories map[string]Memory
}

func (s *hybridMockStore) Save(ctx context.Context, m Memory) error                       { return nil }
func (s *hybridMockStore) Get(ctx context.Context, id string) (*Memory, error) {
	if m, ok := s.memories[id]; ok {
		return &m, nil
	}
	return nil, nil
}
func (s *hybridMockStore) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	return nil, nil
}
func (s *hybridMockStore) Delete(ctx context.Context, id string) error                     { return nil }
func (s *hybridMockStore) List(ctx context.Context, opts ListOptions) ([]Memory, error)   { return nil, nil }
func (s *hybridMockStore) Stats(ctx context.Context) (*StoreStats, error)                  { return nil, nil }
func (s *hybridMockStore) UpdateEmbedding(ctx context.Context, id string, e []float32) error {
	return nil
}
func (s *hybridMockStore) DB() *sql.DB  { return nil }
func (s *hybridMockStore) Close() error { return nil }
func (s *hybridMockStore) ListWithoutEmbedding(ctx context.Context, limit int) ([]Memory, error) { return nil, nil }
func (s *hybridMockStore) Update(ctx context.Context, id, content, category string) error { return nil }
func (s *hybridMockStore) SoftDelete(ctx context.Context, id string) error                   { return nil }
func (s *hybridMockStore) DeleteByScope(ctx context.Context, opts DeleteScopeOptions) (int, error) {
	return 0, nil
}
func (s *hybridMockStore) FindByContentHash(ctx context.Context, hash string, projectID *string) (*Memory, error) {
	return nil, nil
}
func (s *hybridMockStore) BackfillContentHash(ctx context.Context) (int, error) { return 0, nil }
func (s *hybridMockStore) VectorCache() *VectorCache                            { return nil }

type hybridMockEmbedder struct{}

func (m *hybridMockEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (m *hybridMockEmbedder) Dimensions() int { return 0 }
func (m *hybridMockEmbedder) Name() string    { return "mock" }
func (m *hybridMockEmbedder) Model() string   { return "mock" }
func (m *hybridMockEmbedder) Close() error    { return nil }

func TestSearch_BackwardCompatWithoutOpts(t *testing.T) {
	pid := projectIDPtr("proj-1")
	store := &hybridMockStore{
		memories: map[string]Memory{
			"1": {ID: "1", ProjectID: pid, Content: "test", CreatedAt: time.Now()},
		},
	}

	cfg := DefaultHybridSearchConfig()
	cfg.MMREnabled = false
	cfg.TemporalDecayEnabled = false

	h := NewHybridSearcher(store, &hybridMockEmbedder{}, nil, nil, cfg, nil, nil, nil)
	_, err := h.Search(context.Background(), "test")
	if err != nil {
		t.Errorf("Search without opts failed: %v", err)
	}
}

type vec4Embedder struct{}

func (m *vec4Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vec := []float32{0.5, 0.5, 0.5, 0.5}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = vec
	}
	return result, nil
}
func (m *vec4Embedder) Dimensions() int { return 4 }
func (m *vec4Embedder) Name() string    { return "vec4mock" }
func (m *vec4Embedder) Model() string   { return "vec4mock" }
func (m *vec4Embedder) Close() error    { return nil }

func TestSearch_CrossProject_GlobalMode(t *testing.T) {
	proj1 := projectIDPtr("proj-1")
	proj2 := projectIDPtr("proj-2")
	store := &hybridMockStore{
		memories: map[string]Memory{
			"1": {ID: "1", ProjectID: proj1, Content: "golang testing", CreatedAt: time.Now()},
			"2": {ID: "2", ProjectID: proj2, Content: "python testing", CreatedAt: time.Now()},
		},
	}

	cfg := DefaultHybridSearchConfig()
	cfg.MMREnabled = false
	cfg.TemporalDecayEnabled = false

	vc := NewVectorCache(nil)
	vc.Put("1", []float32{0.5, 0.5, 0.5, 0.5})
	vc.Put("2", []float32{0.5, 0.5, 0.5, 0.5})

	h := NewHybridSearcher(store, &vec4Embedder{}, nil, vc, cfg, nil, nil, nil)

	results, err := h.Search(context.Background(), "testing", SearchOptions{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) < 2 {
		t.Errorf("Expected results from multiple projects, got %d", len(results))
	}
}

func TestSearch_CrossProject_WithBoost(t *testing.T) {
	proj1 := projectIDPtr("proj-1")
	proj2 := projectIDPtr("proj-2")
	now := time.Now()
	store := &hybridMockStore{
		memories: map[string]Memory{
			"1": {ID: "1", ProjectID: proj1, Content: "testing framework", CreatedAt: now},
			"2": {ID: "2", ProjectID: proj2, Content: "testing framework", CreatedAt: now},
		},
	}

	cfg := DefaultHybridSearchConfig()
	cfg.MMREnabled = false
	cfg.TemporalDecayEnabled = false

	vc := NewVectorCache(nil)
	vc.Put("1", []float32{0.5, 0.5, 0.5, 0.5})
	vc.Put("2", []float32{-0.5, 0.5, 0.5, 0.5})

	h := NewHybridSearcher(store, &vec4Embedder{}, nil, vc, cfg, nil, nil, nil)

	resultsNoBoost, err := h.Search(context.Background(), "testing", SearchOptions{})
	if err != nil {
		t.Fatalf("Search without boost failed: %v", err)
	}

	resultsWithBoost, err := h.Search(context.Background(), "testing", SearchOptions{BoostProjectID: "proj-1"})
	if err != nil {
		t.Fatalf("Search with boost failed: %v", err)
	}

	if len(resultsWithBoost) < 2 || len(resultsNoBoost) < 2 {
		t.Fatalf("Expected 2+ results, got boosted=%d unboosted=%d", len(resultsWithBoost), len(resultsNoBoost))
	}

	baseScores := map[string]float64{}
	for _, r := range resultsNoBoost {
		baseScores[r.Memory.ID] = r.Score
	}

	boostedScores := map[string]float64{}
	for _, r := range resultsWithBoost {
		boostedScores[r.Memory.ID] = r.Score
	}

	boostedRatio := boostedScores["1"] / baseScores["1"]
	if boostedRatio < 1.29 || boostedRatio > 1.31 {
		t.Errorf("proj-1 boost ratio should be ~1.3, got %f (boosted=%f base=%f)", boostedRatio, boostedScores["1"], baseScores["1"])
	}

	otherRatio := boostedScores["2"] / baseScores["2"]
	if otherRatio < 0.99 || otherRatio > 1.01 {
		t.Errorf("proj-2 should be unchanged, ratio=%f", otherRatio)
	}
}

func TestSearch_CrossProject_BackwardCompat(t *testing.T) {
	pid := projectIDPtr("proj-1")
	store := &hybridMockStore{
		memories: map[string]Memory{
			"1": {ID: "1", ProjectID: pid, Content: "test", CreatedAt: time.Now()},
		},
	}

	cfg := DefaultHybridSearchConfig()
	cfg.MMREnabled = false
	cfg.TemporalDecayEnabled = false

	h := NewHybridSearcher(store, &hybridMockEmbedder{}, nil, nil, cfg, nil, nil, nil)
	results, err := h.Search(context.Background(), "test", SearchOptions{ProjectID: "proj-1"})
	if err != nil {
		t.Errorf("Backward compat search failed: %v", err)
	}
	_ = results
}
