package eval

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type evalEngine struct {
	ranked map[string][]string
}

func (e evalEngine) SearchStreams(_ context.Context, query string, streams []RetrievalStream, _ int) ([]string, error) {
	key := query
	for _, stream := range streams {
		key += ":" + string(stream)
	}
	return e.ranked[key], nil
}

func TestRunRetrievalAblationDeclaresStreamsAndMetrics(t *testing.T) {
	engine := evalEngine{ranked: map[string][]string{
		"graph:kg": {"noise", "m1", "m1"},
	}}
	got, err := RunRetrievalAblation(context.Background(), "kg-only", engine,
		[]RankedQuery{{Name: "graph", Query: "graph", Expected: []string{"m1"}}},
		[]RetrievalStream{StreamKG, StreamKG}, 3,
		RetrievalRunMetadata{CorpusSize: 12, FTS5Available: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Streams, []RetrievalStream{StreamKG}) {
		t.Fatalf("streams=%v", got.Streams)
	}
	if got.K != 3 {
		t.Fatalf("k=%d", got.K)
	}
	if got.Mean.Recall != 1 || got.Mean.MRR != 0.5 || got.Mean.NDCG <= 0 {
		t.Fatalf("metrics=%+v", got.Mean)
	}
	if got.Metadata.CorpusSize != 12 || !got.Metadata.FTS5Available {
		t.Fatalf("metadata=%+v", got.Metadata)
	}
}

func TestRunRetrievalAblationRejectsAmbiguousInput(t *testing.T) {
	engine := evalEngine{ranked: map[string][]string{}}
	metadata := RetrievalRunMetadata{CorpusSize: 1, FTS5Available: true}
	if _, err := RunRetrievalAblation(context.Background(), "bad-stream", engine,
		[]RankedQuery{{Name: "query", Query: "query"}}, []RetrievalStream{"typo"}, 3, metadata); err == nil {
		t.Fatal("unknown stream was accepted")
	}
	if _, err := RunRetrievalAblation(context.Background(), "empty-name", engine,
		[]RankedQuery{{Query: "query"}}, []RetrievalStream{StreamBM25}, 3, metadata); err == nil {
		t.Fatal("empty query name was accepted")
	}
}

func TestTemporalLeakageChecksBothAxes(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Hour)
	got := TemporalLeakage([]TemporalCandidate{
		{ID: "visible", ValidFrom: t0, SystemFrom: t0},
		{ID: "future-valid", ValidFrom: t2, SystemFrom: t0},
		{ID: "future-known", ValidFrom: t0, SystemFrom: t2},
	}, t1, t1)
	if !reflect.DeepEqual(got, []string{"future-known", "future-valid"}) {
		t.Fatalf("leakage=%v", got)
	}
}

func TestVisibilityMilestones(t *testing.T) {
	start := time.Now().UTC()
	vector := start.Add(25 * time.Millisecond)
	remote := start.Add(90 * time.Millisecond)
	got := (VisibilityMilestones{CanonicalAt: start, VectorAt: &vector, RemoteAt: &remote}).Latencies()
	if got["canonical"] != 0 || got["vector"] != 25*time.Millisecond || got["remote"] != 90*time.Millisecond {
		t.Fatalf("latencies=%v", got)
	}
	if _, ok := got["kg"]; ok {
		t.Fatal("missing milestone should not be reported")
	}
}

func TestScoreRankingDoesNotDoubleCountDuplicateHits(t *testing.T) {
	score := ScoreRanking([]string{"expected"}, []string{"expected", "expected"}, 2)
	if score.Recall != 1 || score.MRR != 1 || score.NDCG != 1 {
		t.Fatalf("duplicate hit inflated metrics: %+v", score)
	}
	topK := ScoreRanking([]string{"m1", "m2"}, []string{"m1", "m1", "m2"}, 2)
	if topK.Recall != 0.5 {
		t.Fatalf("duplicate did not consume top-k position: %+v", topK)
	}
}
