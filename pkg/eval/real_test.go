package eval

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
)

// fakeSearcher answers each query with a fixed ranking of fixture keys, so the
// scoring and aggregation can be checked without a model.
type fakeSearcher struct {
	ids     map[string]string // key -> id
	answers map[string][]string
	seeded  []string
}

func (f *fakeSearcher) SaveWithOptions(_ context.Context, o memory.SaveOptions) (*memory.Memory, error) {
	id := fmt.Sprintf("id-%d", len(f.seeded))
	f.seeded = append(f.seeded, o.Content)
	for key, content := range fakeCorpus {
		if content == o.Content {
			f.ids[key] = id
		}
	}
	return &memory.Memory{ID: id}, nil
}
func (f *fakeSearcher) BackfillEmbeddings(context.Context, int) (int, error) {
	return len(f.seeded), nil
}
func (f *fakeSearcher) Search(_ context.Context, q string, _ memory.SearchOptions) ([]memory.SearchResult, error) {
	var out []memory.SearchResult
	for _, key := range f.answers[q] {
		out = append(out, memory.SearchResult{Memory: memory.Memory{ID: f.ids[key]}})
	}
	return out, nil
}

var fakeCorpus = map[string]string{"a": "alpha text", "b": "beta text", "c": "gamma text"}

const fakeFixture = `
memories:
  - {key: a, category: fact, content: "alpha text"}
  - {key: b, category: fact, content: "beta text"}
  - {key: c, category: fact, content: "gamma text"}
queries:
  - {query: "first", expect: [a]}
  - {query: "second", expect: [b]}
`

func TestRunRecallReal_ScoresRankingsFromTheServicePath(t *testing.T) {
	f := &fakeSearcher{ids: map[string]string{}, answers: map[string][]string{
		"first":  {"a", "b"}, // hit at rank 1
		"second": {"c", "b"}, // hit at rank 2
	}}
	res, err := RunRecallReal(context.Background(), f, []byte(fakeFixture), 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Corpus != 3 || res.Embedded != 3 {
		t.Errorf("corpus/embedded = %d/%d, want 3/3", res.Corpus, res.Embedded)
	}
	if res.Runs != 3 || res.RecallSpread != [2]float64{1, 1} {
		t.Errorf("a deterministic ranking repeats exactly: runs=%d spread=%v", res.Runs, res.RecallSpread)
	}
	if res.Metrics.RecallAt5 != 1 {
		t.Errorf("recall@5 = %.3f, want 1", res.Metrics.RecallAt5)
	}
	if got, want := res.Metrics.MRRAt10, (1.0+0.5)/2; got != want {
		t.Errorf("mrr@10 = %.3f, want %.3f", got, want)
	}
}

func TestCompareToBaseline_GatesRecallAndMRR(t *testing.T) {
	base := QualityMetrics{RecallAt5: 0.5, MRRAt10: 0.4, NDCGAt10: 0.45}
	if ok, detail := CompareToBaseline(QualityMetrics{RecallAt5: 0.61, MRRAt10: 0.49, NDCGAt10: 0.1}, base, 1.2); !ok {
		t.Errorf("a 20%% gain on both gated metrics must pass:\n%s", detail)
	}
	ok, detail := CompareToBaseline(QualityMetrics{RecallAt5: 0.7, MRRAt10: 0.41}, base, 1.2)
	if ok || !strings.Contains(detail, "BELOW") {
		t.Errorf("MRR short of the gain must fail:\n%s", detail)
	}
}

func TestMeasureEmbeddingHealth_TellsCollapsedFromSpreadSpaces(t *testing.T) {
	ctx := context.Background()
	measure := func(vecs [][]float32) EmbeddingHealth {
		t.Helper()
		store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "health.db"), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		identity := memory.EmbeddingIdentity{Provider: "test", Model: "h", ModelRevision: "1", Dimensions: 2, Normalization: "l2"}
		gen, err := store.EnsureEmbeddingGeneration(ctx, identity)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range vecs {
			id := fmt.Sprintf("m%d", i)
			content := fmt.Sprintf("memory %d", i)
			rev, err := store.SaveTemporal(ctx, memory.Memory{ID: id, Category: "fact", Content: content}, memory.TemporalWriteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutEmbeddingVector(ctx, memory.EmbeddingVectorRecord{
				RevisionID: rev.RevisionID, MemoryID: id, GenerationID: gen.ID,
				SemanticSpaceID: gen.SemanticSpaceID, Purpose: memory.EmbeddingPurposeDocument,
				Identity: identity, ContentHash: rev.Memory.ContentHash, Vector: v,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.ActivateEmbeddingGeneration(ctx, gen.ID); err != nil {
			t.Fatal(err)
		}
		h, err := MeasureEmbeddingHealth(ctx, store.DB(), 50)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	var collapsed, spread [][]float32
	for i := 0; i < 40; i++ {
		collapsed = append(collapsed, []float32{1, 0.01 * float32(i%3)})
		if i%2 == 0 {
			spread = append(spread, []float32{1, 0})
		} else {
			spread = append(spread, []float32{0, 1})
		}
	}
	if h := measure(collapsed); h.MeanCosine < 0.9 || h.ShareAbove075 < 0.9 {
		t.Errorf("collapsed space must read as collapsed: %+v", h)
	}
	if h := measure(spread); h.MeanCosine > 0.8 || h.Pairs == 0 {
		t.Errorf("a spread space must not read as collapsed: %+v", h)
	}
}

// alternatingSearcher returns a different ranking on each call for the same
// query, like the v0.19 search whose MMR started from a random candidate.
type alternatingSearcher struct {
	fakeSearcher
	calls map[string]int
}

func (a *alternatingSearcher) Search(ctx context.Context, q string, o memory.SearchOptions) ([]memory.SearchResult, error) {
	a.calls[q]++
	if a.calls[q]%2 == 1 {
		return a.fakeSearcher.Search(ctx, q, o)
	}
	return nil, nil // every other run finds nothing
}

func TestRunRecallReal_AveragesRunsAndReportsTheSpread(t *testing.T) {
	a := &alternatingSearcher{
		fakeSearcher: fakeSearcher{ids: map[string]string{}, answers: map[string][]string{"first": {"a"}, "second": {"b"}}},
		calls:        map[string]int{},
	}
	res, err := RunRecallReal(context.Background(), a, []byte(fakeFixture), 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.RecallAt5 != 0.5 {
		t.Errorf("mean recall over alternating runs = %.3f, want 0.5", res.Metrics.RecallAt5)
	}
	if res.RecallSpread != [2]float64{0, 1} || len(res.PerRun) != 4 {
		t.Errorf("spread %v and per-run %v must show the variation", res.RecallSpread, res.PerRun)
	}
	if res.FixtureSHA == "" {
		t.Error("the result must record which fixture produced it")
	}
}

func TestCompareToBaseline_RecallAloneCanFail(t *testing.T) {
	base := QualityMetrics{RecallAt5: 0.5, MRRAt10: 0.4, NDCGAt10: 0.45}
	ok, detail := CompareToBaseline(QualityMetrics{RecallAt5: 0.55, MRRAt10: 0.9, NDCGAt10: 0.9}, base, 1.2)
	if ok || !strings.Contains(detail, "recall@5") || !strings.Contains(detail, "BELOW") {
		t.Errorf("recall short of the gain must fail even with a high MRR:\n%s", detail)
	}
	if ok, detail := CompareToBaseline(QualityMetrics{RecallAt5: 0.6, MRRAt10: 0.48, NDCGAt10: 0}, base, 1.2); !ok {
		t.Errorf("nDCG is reported, not gated:\n%s", detail)
	}
}

func TestCheckComparable_RefusesAnotherFixtureOrModel(t *testing.T) {
	base := RealRecallResult{FixtureSHA: "abc", Model: "m1"}
	if err := CheckComparable(RealRecallResult{FixtureSHA: "abc", Model: "m1"}, base); err != nil {
		t.Errorf("same fixture and model must compare: %v", err)
	}
	if err := CheckComparable(RealRecallResult{FixtureSHA: "zzz", Model: "m1"}, base); err == nil {
		t.Error("a different fixture must refuse the comparison")
	}
	if err := CheckComparable(RealRecallResult{FixtureSHA: "abc", Model: "m2"}, base); err == nil {
		t.Error("a different model must refuse the comparison")
	}
}

func TestCheckCoverage_FailsWhenMemoriesLackVectors(t *testing.T) {
	if err := CheckCoverage(EmbeddingHealth{Vectors: 98}, 98); err != nil {
		t.Errorf("full coverage: %v", err)
	}
	if err := CheckCoverage(EmbeddingHealth{Vectors: 80}, 98); err == nil {
		t.Error("80 of 98 memories embedded must fail: vector recall would be measured on a partial corpus")
	}
}
