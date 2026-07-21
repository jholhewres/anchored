package eval

import (
	"context"
	"fmt"
	"math"

	"github.com/jholhewres/anchored/pkg/memory"
)

// RecallStore is the slice of a memory store the recall eval needs: seed a
// corpus and search it. *memory.SQLiteStore satisfies this.
type RecallStore interface {
	Save(ctx context.Context, m memory.Memory) error
	Search(ctx context.Context, query string, opts memory.SearchOptions) ([]memory.SearchResult, error)
}

// RunRecall seeds the fixture's corpus into store, runs each query, and scores
// Recall@K against the expected memories. Recall@K = |retrieved∩expected| /
// |expected|. The run passes when every query meets its minimum recall.
func RunRecall(ctx context.Context, store RecallStore, fixture []byte) (Report, error) {
	var fix RecallFixture
	if err := parseYAML(fixture, &fix); err != nil {
		return Report{}, err
	}
	k := fix.K
	if k <= 0 {
		k = 5
	}
	floor := fix.MinRecall
	if floor <= 0 {
		floor = 0.8
	}

	// Seed with the fixture key as the memory id (Save honors a provided id), so
	// expected keys compare directly against retrieved ids — no fragile
	// content-based id resolution.
	for _, m := range fix.Memories {
		if err := store.Save(ctx, memory.Memory{
			ID:       m.Key,
			Category: m.Category,
			Content:  m.Content,
			Keywords: m.Keywords,
			Source:   "eval",
		}); err != nil {
			return Report{}, fmt.Errorf("seed %s: %w", m.Key, err)
		}
	}

	rep := Report{Name: "recall"}
	var total float64
	for _, q := range fix.Queries {
		want := map[string]bool{}
		for _, key := range q.Expect {
			want[key] = true // seeded id == fixture key
		}
		// Mirror the production BM25 path (HybridSearcher.searchBM25): expand the
		// free-form query into a safe FTS expression (OR-joined, accent-folded,
		// escaped) instead of passing it raw to FTS5 MATCH.
		ftsQuery := memory.ExpandQueryAdvanced(q.Query)
		if ftsQuery == "" {
			ftsQuery = memory.ExpandQueryForFTS(memory.ExtractKeywords(q.Query))
		}
		var res []memory.SearchResult
		if ftsQuery != "" {
			r, err := store.Search(ctx, ftsQuery, memory.SearchOptions{MaxResults: k})
			if err != nil {
				return Report{}, fmt.Errorf("search %q: %w", q.Query, err)
			}
			res = r
		}
		rankedIDs := make([]string, 0, k)
		for i, r := range res {
			if i >= k {
				break
			}
			rankedIDs = append(rankedIDs, r.Memory.ID)
		}
		expectedIDs := make([]string, 0, len(want))
		for id := range want {
			expectedIDs = append(expectedIDs, id)
		}
		ranking := ScoreRanking(expectedIDs, rankedIDs, k)
		recall := ranking.Recall
		hit := int(recall*float64(len(want)) + 0.5)
		min := q.MinRecall
		if min <= 0 {
			min = floor
		}
		passed := recall >= min
		total += recall
		rep.Cases = append(rep.Cases, CaseResult{
			Name:   q.Query,
			Passed: passed,
			Score:  recall,
			Detail: fmt.Sprintf("recall@%d=%.2f (min %.2f), mrr=%.2f, ndcg@%d=%.2f, %d/%d expected", k, recall, min, ranking.MRR, k, ranking.NDCG, hit, len(want)),
		})
	}

	rep.Passed = true
	for _, c := range rep.Cases {
		if !c.Passed {
			rep.Passed = false
		}
	}
	if len(rep.Cases) > 0 {
		rep.Score = total / float64(len(rep.Cases))
	}
	rep.Summary = fmt.Sprintf("%d queries, mean recall@%d=%.2f", len(rep.Cases), k, rep.Score)
	return rep, nil
}

func reciprocalRank(rankedRelevant []bool) float64 {
	for i, relevant := range rankedRelevant {
		if relevant {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func normalizedDiscountedCumulativeGain(rankedRelevant []bool, relevantTotal, k int) float64 {
	if relevantTotal <= 0 {
		return 1
	}
	if k <= 0 {
		return 0
	}
	var dcg float64
	for i, relevant := range rankedRelevant {
		if i >= k {
			break
		}
		if relevant {
			dcg += 1 / math.Log2(float64(i+2))
		}
	}
	idealCount := relevantTotal
	if idealCount > k {
		idealCount = k
	}
	var ideal float64
	for i := 0; i < idealCount; i++ {
		ideal += 1 / math.Log2(float64(i+2))
	}
	if ideal == 0 {
		return 0
	}
	return dcg / ideal
}
