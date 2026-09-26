package eval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/jholhewres/anchored/pkg/memory"
)

// RealSearcher is the production path the real-embedder eval drives: save
// through the service, embed synchronously, search with the full hybrid
// pipeline (vector + BM25 fusion, MMR, decay). *memory.Service satisfies it.
type RealSearcher interface {
	SaveWithOptions(ctx context.Context, opts memory.SaveOptions) (*memory.Memory, error)
	BackfillEmbeddings(ctx context.Context, batchSize int) (int, error)
	Search(ctx context.Context, query string, opts memory.SearchOptions) ([]memory.SearchResult, error)
}

// QualityMetrics are the retrieval numbers the release gates on.
type QualityMetrics struct {
	RecallAt5 float64 `json:"recall_at_5"`
	MRRAt10   float64 `json:"mrr_at_10"`
	NDCGAt10  float64 `json:"ndcg_at_10"`
}

// RealRecallResult is a real-embedder run: the aggregate metrics, the
// per-query breakdown and what produced them.
type RealRecallResult struct {
	Report
	Metrics  QualityMetrics            `json:"metrics"`
	PerQuery map[string]QualityMetrics `json:"per_query"`
	Corpus   int                       `json:"corpus"`
	Embedded int                       `json:"embedded"`
	Label    string                    `json:"label,omitempty"`
	// Runs is how many times the queries were repeated over the same corpus;
	// the metrics are their mean. RecallSpread is the lowest and highest
	// per-run recall@5: a spread above zero means the ranking is not
	// deterministic (as in v0.19.2, where MMR started from a random pick).
	Runs         int        `json:"runs"`
	RecallSpread [2]float64 `json:"recall_spread"`
	PerRun       []float64  `json:"per_run_recall_at_5"`
	// FixtureSHA and Model tie a result to what produced it: a comparison
	// across fixtures or models would measure the change of corpus, not of
	// search.
	FixtureSHA string `json:"fixture_sha256"`
	Model      string `json:"model,omitempty"`
	// Health is the eval corpus's own vector space, filled by the caller
	// that owns the database.
	Health EmbeddingHealth `json:"health"`
}

// RunRecallReal seeds the fixture through the service, waits for every
// embedding, then scores each query's ranking — recall@5, and MRR and nDCG
// over the top 10 — averaged over runs repetitions of the query set.
func RunRecallReal(ctx context.Context, s RealSearcher, fixture []byte, runs int) (RealRecallResult, error) {
	var fix RecallFixture
	if err := parseYAML(fixture, &fix); err != nil {
		return RealRecallResult{}, err
	}
	if runs <= 0 {
		runs = 1
	}
	keyByID := make(map[string]string, len(fix.Memories))
	for _, m := range fix.Memories {
		saved, err := s.SaveWithOptions(ctx, memory.SaveOptions{
			Content: m.Content, Category: m.Category, Source: "eval", SkipEmbed: true,
		})
		if err != nil {
			return RealRecallResult{}, fmt.Errorf("seed %s: %w", m.Key, err)
		}
		keyByID[saved.ID] = m.Key
	}
	embedded, err := s.BackfillEmbeddings(ctx, 64)
	if err != nil {
		return RealRecallResult{}, fmt.Errorf("embed corpus: %w", err)
	}

	res := RealRecallResult{
		Report:     Report{Name: "recall-real", Passed: true},
		PerQuery:   make(map[string]QualityMetrics, len(fix.Queries)),
		Corpus:     len(fix.Memories),
		Embedded:   embedded,
		Runs:       runs,
		FixtureSHA: fmt.Sprintf("%x", sha256.Sum256(fixture)),
	}
	lastTop := make(map[string][]string, len(fix.Queries))
	for run := 0; run < runs; run++ {
		var runRecall float64
		for _, q := range fix.Queries {
			results, err := s.Search(ctx, q.Query, memory.SearchOptions{MaxResults: 10})
			if err != nil {
				return RealRecallResult{}, fmt.Errorf("search %q: %w", q.Query, err)
			}
			ranked := make([]string, 0, len(results))
			for _, r := range results {
				ranked = append(ranked, keyByID[r.Memory.ID])
			}
			at5 := ScoreRanking(q.Expect, ranked, 5)
			at10 := ScoreRanking(q.Expect, ranked, 10)
			m := res.PerQuery[q.Query]
			m.RecallAt5 += at5.Recall / float64(runs)
			m.MRRAt10 += at10.MRR / float64(runs)
			m.NDCGAt10 += at10.NDCG / float64(runs)
			res.PerQuery[q.Query] = m
			runRecall += at5.Recall
			lastTop[q.Query] = head(ranked, 3)
		}
		if n := float64(len(fix.Queries)); n > 0 {
			runRecall /= n
		}
		res.PerRun = append(res.PerRun, runRecall)
		if run == 0 || runRecall < res.RecallSpread[0] {
			res.RecallSpread[0] = runRecall
		}
		if run == 0 || runRecall > res.RecallSpread[1] {
			res.RecallSpread[1] = runRecall
		}
	}

	var sum QualityMetrics
	for _, q := range fix.Queries {
		m := res.PerQuery[q.Query]
		sum.RecallAt5 += m.RecallAt5
		sum.MRRAt10 += m.MRRAt10
		sum.NDCGAt10 += m.NDCGAt10
		res.Cases = append(res.Cases, CaseResult{
			Name: q.Query, Passed: true, Score: m.RecallAt5,
			Detail: fmt.Sprintf("recall@5=%.2f mrr@10=%.2f ndcg@10=%.2f top=%v", m.RecallAt5, m.MRRAt10, m.NDCGAt10, lastTop[q.Query]),
		})
	}
	if n := float64(len(fix.Queries)); n > 0 {
		res.Metrics = QualityMetrics{RecallAt5: sum.RecallAt5 / n, MRRAt10: sum.MRRAt10 / n, NDCGAt10: sum.NDCGAt10 / n}
	}
	res.Score = res.Metrics.RecallAt5
	res.Summary = fmt.Sprintf("%d queries over %d memories, %d runs: recall@5=%.3f (%.3f-%.3f) mrr@10=%.3f ndcg@10=%.3f",
		len(fix.Queries), res.Corpus, runs, res.Metrics.RecallAt5, res.RecallSpread[0], res.RecallSpread[1],
		res.Metrics.MRRAt10, res.Metrics.NDCGAt10)
	return res, nil
}

func head(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// CompareToBaseline checks that recall@5 and MRR@10 reached baseline×minGain.
// nDCG is reported but not gated: it moves with the other two.
func CompareToBaseline(current, baseline QualityMetrics, minGain float64) (bool, string) {
	pass := true
	line := func(name string, cur, base float64, gated bool) string {
		need := base * minGain
		mark := "ok"
		if gated && cur+1e-9 < need {
			mark, pass = "BELOW", false
		}
		if !gated {
			mark = "info"
		}
		return fmt.Sprintf("  %-10s %.3f -> %.3f (%+.1f%%, need >= %.3f) %s", name, base, cur, pct(cur, base), need, mark)
	}
	detail := line("recall@5", current.RecallAt5, baseline.RecallAt5, true) + "\n" +
		line("mrr@10", current.MRRAt10, baseline.MRRAt10, true) + "\n" +
		line("ndcg@10", current.NDCGAt10, baseline.NDCGAt10, false)
	return pass, detail
}

func pct(cur, base float64) float64 {
	if base == 0 {
		return 0
	}
	return (cur - base) / base * 100
}

// CheckComparable refuses a comparison between results of different fixtures
// or embedding models.
func CheckComparable(current, baseline RealRecallResult) error {
	if baseline.FixtureSHA != "" && current.FixtureSHA != baseline.FixtureSHA {
		return fmt.Errorf("baseline was measured on another fixture (sha256 %.12s, now %.12s)", baseline.FixtureSHA, current.FixtureSHA)
	}
	if baseline.Model != "" && current.Model != baseline.Model {
		return fmt.Errorf("baseline was measured with model %q, now %q", baseline.Model, current.Model)
	}
	return nil
}

// CheckCoverage fails a run where some corpus memories have no vector in the
// active space: vector recall would be measured on part of the corpus.
func CheckCoverage(h EmbeddingHealth, corpus int) error {
	if h.Vectors < corpus {
		return fmt.Errorf("only %d of %d memories have a vector in the active space", h.Vectors, corpus)
	}
	return nil
}

// LoadRealResult reads a result saved with --save-baseline.
func LoadRealResult(path string) (RealRecallResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RealRecallResult{}, err
	}
	var r RealRecallResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return RealRecallResult{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return r, nil
}

// EmbeddingHealth describes how spread out a vector space is. In a healthy
// sentence-embedding space unrelated texts sit far apart (mean cosine of
// random pairs around 0.1-0.3); a space where everything looks alike (0.7+)
// cannot rank by meaning.
type EmbeddingHealth struct {
	Vectors       int     `json:"vectors"`
	Pairs         int     `json:"pairs"`
	MeanCosine    float64 `json:"mean_cosine"`
	MedianCosine  float64 `json:"median_cosine"`
	ShareAbove075 float64 `json:"share_above_0_75"`
}

// MeasureEmbeddingHealth samples random pairs of the active generation's
// document vectors for live memories. It only reads.
func MeasureEmbeddingHealth(ctx context.Context, db *sql.DB, pairs int) (EmbeddingHealth, error) {
	if pairs <= 0 {
		pairs = 1500
	}
	rows, err := db.QueryContext(ctx, `
		SELECT v.embedding
		FROM memory_embedding_vectors v
		JOIN embedding_generations g ON g.generation_id = v.generation_id AND g.state = 'active'
		JOIN memories m ON m.current_revision_id = v.revision_id AND m.deleted_at IS NULL
		WHERE v.purpose = 'document'
		ORDER BY random()
		LIMIT ?`, pairs*2)
	if err != nil {
		return EmbeddingHealth{}, fmt.Errorf("sample vectors: %w", err)
	}
	defer rows.Close()
	var vecs [][]float32
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return EmbeddingHealth{}, err
		}
		if v := decodeVector(blob); v != nil {
			vecs = append(vecs, v)
		}
	}
	if err := rows.Err(); err != nil {
		return EmbeddingHealth{}, err
	}
	h := EmbeddingHealth{Vectors: len(vecs)}
	var sims []float64
	for i := 0; i+1 < len(vecs); i += 2 {
		sims = append(sims, cosine(vecs[i], vecs[i+1]))
	}
	if len(sims) == 0 {
		return h, nil
	}
	sort.Float64s(sims)
	var sum float64
	above := 0
	for _, s := range sims {
		sum += s
		if s >= 0.75 {
			above++
		}
	}
	h.Pairs = len(sims)
	h.MeanCosine = sum / float64(len(sims))
	h.MedianCosine = sims[len(sims)/2]
	h.ShareAbove075 = float64(above) / float64(len(sims))
	return h, nil
}

func decodeVector(blob []byte) []float32 {
	if len(blob) == 0 || len(blob)%4 != 0 {
		return nil
	}
	v := make([]float32, len(blob)/4)
	for i := range v {
		bits := uint32(blob[i*4]) | uint32(blob[i*4+1])<<8 | uint32(blob[i*4+2])<<16 | uint32(blob[i*4+3])<<24
		v[i] = math.Float32frombits(bits)
	}
	return v
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
