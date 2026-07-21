package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type purposeRecordingEmbedder struct {
	mu              sync.Mutex
	documentCalls   int
	queryCalls      int
	fallbackCalls   int
	dimensions      int
	returnDimension int
	firstDocument   chan struct{}
	releaseFirst    chan struct{}
}

func (e *purposeRecordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.fallbackCalls++
	e.mu.Unlock()
	return e.vectors(texts), nil
}

func (e *purposeRecordingEmbedder) EmbedPurpose(
	ctx context.Context,
	purpose EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	e.mu.Lock()
	switch purpose {
	case EmbeddingPurposeDocument:
		e.documentCalls++
		call := e.documentCalls
		first := e.firstDocument
		release := e.releaseFirst
		e.mu.Unlock()
		if call == 1 && first != nil {
			close(first)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
		}
	case EmbeddingPurposeQuery:
		e.queryCalls++
		e.mu.Unlock()
	default:
		e.mu.Unlock()
	}
	return e.vectors(texts), nil
}

func (e *purposeRecordingEmbedder) vectors(texts []string) [][]float32 {
	dimensions := e.returnDimension
	if dimensions == 0 {
		dimensions = e.dimensions
	}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = make([]float32, dimensions)
		if dimensions > 0 {
			result[i][0] = 1
		}
	}
	return result
}

func (e *purposeRecordingEmbedder) Dimensions() int { return e.dimensions }
func (e *purposeRecordingEmbedder) Name() string    { return "purpose-test" }
func (e *purposeRecordingEmbedder) Model() string   { return "purpose-test-v1" }
func (e *purposeRecordingEmbedder) Close() error    { return nil }

func (e *purposeRecordingEmbedder) calls() (document, query, fallback int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.documentCalls, e.queryCalls, e.fallbackCalls
}

func newGenerationTestService(
	t *testing.T,
	store *SQLiteStore,
	embedder EmbeddingProvider,
) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := &Service{
		store:    store,
		embedder: embedder,
		logger:   logger,
		embedSem: make(chan struct{}, 1),
		shutdown: make(chan struct{}),
	}
	config := DefaultHybridSearchConfig()
	config.MMREnabled = false
	config.TemporalDecayEnabled = false
	service.searcher = NewHybridSearcher(
		store, embedder, nil, store.VectorCache(), config, nil, nil, logger,
	)
	if err := service.ensureCurrentEmbeddingGeneration(context.Background()); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestGenerationWorkerResumesAndUsesDocumentAndQueryPurposes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "resume-generation.db")
	first, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Save(ctx, Memory{
		ID: "resume", Content: "durable semantic restart",
		Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	identity := EmbeddingIdentity{
		Provider: "purpose-test", Model: "purpose-test-v1",
		Dimensions: 2, Normalization: "l2",
	}
	generation, err := first.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.EnsureEmbeddingGenerationJobs(ctx, generation.ID, 0); err != nil {
		t.Fatal(err)
	}
	staleNow := time.Now().UTC().Add(-2 * time.Minute)
	claimed, err := first.ClaimProcessingJob(
		ctx, embeddingJobKind, "interrupted-worker", staleNow, time.Minute,
	)
	if err != nil || claimed == nil {
		t.Fatalf("claim interrupted job = (%+v, %v)", claimed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &purposeRecordingEmbedder{dimensions: 2}
	service := newGenerationTestService(t, reopened, embedder)
	defer service.Close()
	service.ensureDurableWorkers()

	waitForActiveGeneration(t, reopened, generation.ID)
	results, err := service.searcher.searchVector(ctx, "restart", 10, nil, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Memory.ID != "resume" {
		t.Fatalf("semantic results = %+v", results)
	}
	if reopened.VectorCache().Len() != 1 {
		t.Fatalf("published generation cache size = %d, want 1", reopened.VectorCache().Len())
	}
	document, query, fallback := embedder.calls()
	if document == 0 || query != 1 || fallback != 0 {
		t.Fatalf("purpose calls document=%d query=%d fallback=%d", document, query, fallback)
	}
}

func TestGenerationWorkerCapturesConcurrentDeltaBeforeActivation(t *testing.T) {
	ctx := context.Background()
	store := newTemporalTestStore(t)
	if err := store.Save(ctx, Memory{
		ID: "snapshot", Content: "snapshot memory", Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	embedder := &purposeRecordingEmbedder{
		dimensions:    2,
		firstDocument: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	service := newGenerationTestService(t, store, embedder)
	defer service.Close()
	generationID := service.currentEmbeddingGenerationID()
	service.ensureDurableWorkers()

	select {
	case <-embedder.firstDocument:
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot embedding did not start")
	}
	if _, err := service.SaveWithOptions(ctx, SaveOptions{
		Content: "delta while generation is building", Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	close(embedder.releaseFirst)

	waitForActiveGeneration(t, store, generationID)
	if store.VectorCache().Len() != 2 {
		t.Fatalf("active generation cache size = %d, want snapshot + delta", store.VectorCache().Len())
	}
	missing, err := store.CountMissingEmbeddingRevisions(ctx, generationID)
	if err != nil || missing != 0 {
		t.Fatalf("generation missing = %d, err=%v", missing, err)
	}
}

func TestGenerationVectorsFollowContentStableRevisionsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stable-revisions.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	embedder := &purposeRecordingEmbedder{dimensions: 2}
	service := newGenerationTestService(t, store, embedder)
	service.ensureDurableWorkers()
	saved, err := service.SaveWithOptions(ctx, SaveOptions{
		Content:  "stable content with category and metadata changes",
		Category: "fact",
		Source:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	generationID := service.currentEmbeddingGenerationID()
	waitForCurrentGenerationVector(t, store, saved.ID, generationID)

	if _, err := service.Update(ctx, saved.ID, "", "decision"); err != nil {
		t.Fatal(err)
	}
	waitForCurrentGenerationVector(t, store, saved.ID, generationID)

	if err := service.UpdateMetadata(ctx, saved.ID, map[string]any{"reviewed": true}); err != nil {
		t.Fatal(err)
	}
	waitForCurrentGenerationVector(t, store, saved.ID, generationID)

	if err := service.SoftForget(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Restore(ctx, saved.ID); err != nil {
		t.Fatal(err)
	}
	waitForCurrentGenerationVector(t, store, saved.ID, generationID)

	service.Close()
	reopened, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok := reopened.VectorCache().Get(saved.ID); !ok || len(got) != 2 {
		t.Fatalf("generation vector missing after restart: vector=%v ok=%v", got, ok)
	}
}

func waitForCurrentGenerationVector(t *testing.T, store *SQLiteStore, memoryID, generationID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		err := store.DB().QueryRow(`SELECT COUNT(*)
			FROM memories m
			JOIN memory_embedding_vectors v
			  ON v.revision_id = m.current_revision_id
			 AND v.generation_id = ?
			 AND v.purpose = 'document'
			WHERE m.id = ?`, generationID, memoryID).Scan(&count)
		if err == nil && count == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("current revision for %s never received generation %s vector", memoryID, generationID)
}

func TestGenerationAwareQueryRejectsProviderDimensionMismatch(t *testing.T) {
	embedder := &purposeRecordingEmbedder{dimensions: 2, returnDimension: 1}
	cache := NewVectorCache(nil)
	cache.Put("m1", []float32{1, 0})
	searcher := NewHybridSearcher(
		&hybridMockStore{}, embedder, nil, cache,
		DefaultHybridSearchConfig(), nil, nil, nil,
	)
	identity, err := EmbeddingIdentityOf(embedder)
	if err != nil {
		t.Fatal(err)
	}
	searcher.UseEmbeddingGeneration(&identity)
	_, err = searcher.searchVector(
		context.Background(), "wrong dimensions", 10, nil, SearchOptions{},
	)
	if !errors.Is(err, ErrEmbeddingDimensionMismatch) {
		t.Fatalf("dimension mismatch error = %v", err)
	}
}

func TestIncompatibleActiveGenerationIsNotSearchedWhileCurrentBuilds(t *testing.T) {
	ctx := context.Background()
	store := newTemporalTestStore(t)
	if err := store.Save(ctx, Memory{
		ID: "old-space", Content: "vector from old model",
		Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	revision, err := store.RevisionAt(ctx, "old-space", TemporalQueryOptions{})
	if err != nil || revision == nil {
		t.Fatalf("revision = (%+v, %v)", revision, err)
	}
	oldIdentity := EmbeddingIdentity{
		Provider: "other", Model: "same-width-other-model",
		Dimensions: 2, Normalization: "l2",
	}
	oldGeneration, err := store.EnsureEmbeddingGeneration(ctx, oldIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: revision.RevisionID, MemoryID: revision.MemoryID,
		GenerationID: oldGeneration.ID, SemanticSpaceID: oldGeneration.SemanticSpaceID,
		Purpose: EmbeddingPurposeDocument, Identity: oldIdentity,
		ContentHash: revision.Memory.ContentHash, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, oldGeneration.ID); err != nil {
		t.Fatal(err)
	}

	embedder := &purposeRecordingEmbedder{dimensions: 2}
	service := newGenerationTestService(t, store, embedder)
	defer service.Close()
	if store.VectorCache().Len() != 0 {
		t.Fatal("incompatible active vectors leaked into the current search space")
	}
	results, err := service.searcher.searchVector(ctx, "old model", 10, nil, SearchOptions{})
	if err != nil || len(results) != 0 {
		t.Fatalf("building generation vector search = (%+v, %v)", results, err)
	}
	_, query, _ := embedder.calls()
	if query != 0 {
		t.Fatalf("query embedding calls while generation builds = %d, want 0", query)
	}
}

func TestGenerationBackfillIsBoundedResumableAndActivates(t *testing.T) {
	ctx := context.Background()
	store := newTemporalTestStore(t)
	for _, id := range []string{"b1", "b2", "b3"} {
		if err := store.Save(ctx, Memory{
			ID: id, Content: "backfill " + id, Category: "fact", Source: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	embedder := &purposeRecordingEmbedder{dimensions: 2}
	service := newGenerationTestService(t, store, embedder)
	defer service.Close()
	generationID := service.currentEmbeddingGenerationID()

	embedded, err := service.BackfillEmbeddingsLimited(ctx, 2, 0, 2)
	if err != nil || embedded != 2 {
		t.Fatalf("bounded backfill = (%d, %v), want (2, nil)", embedded, err)
	}
	missing, err := service.PendingEmbeddings(ctx)
	if err != nil || missing != 1 {
		t.Fatalf("pending after bounded backfill = (%d, %v), want (1, nil)", missing, err)
	}
	generation, err := store.EmbeddingGeneration(ctx, generationID)
	if err != nil || generation == nil || generation.State != EmbeddingGenerationBuilding {
		t.Fatalf("partial generation = (%+v, %v)", generation, err)
	}

	embedded, err = service.BackfillEmbeddings(ctx, 2)
	if err != nil || embedded != 1 {
		t.Fatalf("resumed backfill = (%d, %v), want (1, nil)", embedded, err)
	}
	waitForActiveGeneration(t, store, generationID)
	if store.VectorCache().Len() != 3 {
		t.Fatalf("activated backfill cache size = %d, want 3", store.VectorCache().Len())
	}
	document, _, fallback := embedder.calls()
	if document != 3 || fallback != 0 {
		t.Fatalf("backfill calls document=%d fallback=%d", document, fallback)
	}
}

func waitForActiveGeneration(t *testing.T, store *SQLiteStore, generationID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		generation, err := store.ActiveEmbeddingGeneration(context.Background())
		if err == nil && generation != nil && generation.ID == generationID {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	generation, err := store.EmbeddingGeneration(context.Background(), generationID)
	t.Fatalf("generation did not activate: generation=%+v err=%v", generation, err)
}
