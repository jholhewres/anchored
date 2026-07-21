package memory

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEmbeddingGenerationActivationIsCoverageGatedAndAtomic(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "generations.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(ctx, Memory{ID: "m1", Content: "first revision", ContentHash: contentHash("first revision"), Category: "fact", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	current, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{})
	if err != nil || current == nil {
		t.Fatalf("current=%v err=%v", current, err)
	}
	identityA := EmbeddingIdentity{Provider: "test", Model: "a", ModelRevision: "1", Dimensions: 2, Normalization: "l2"}
	genA, err := store.EnsureEmbeddingGeneration(ctx, identityA)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, genA.ID); !errors.Is(err, ErrEmbeddingGenerationIncomplete) {
		t.Fatalf("activate incomplete error=%v", err)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: current.RevisionID, MemoryID: "m1", GenerationID: genA.ID,
		SemanticSpaceID: genA.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identityA, ContentHash: current.Memory.ContentHash, Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, genA.ID); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.VectorCache().Get("m1"); !ok || got[0] != 1 {
		t.Fatalf("active vector=%v ok=%v", got, ok)
	}

	identityB := EmbeddingIdentity{Provider: "test", Model: "b", ModelRevision: "1", Dimensions: 2, Normalization: "l2"}
	genB, err := store.EnsureEmbeddingGeneration(ctx, identityB)
	if err != nil {
		t.Fatal(err)
	}
	// A same-width model does not replace the active space until its complete
	// generation is activated.
	if active, _ := store.ActiveEmbeddingGeneration(ctx); active == nil || active.ID != genA.ID {
		t.Fatalf("active generation changed during build: %+v", active)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: current.RevisionID, MemoryID: "m1", GenerationID: genB.ID,
		SemanticSpaceID: genB.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identityB, ContentHash: current.Memory.ContentHash, Vector: []float32{0, 1},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.VectorCache().Get("m1"); got[0] != 1 {
		t.Fatalf("building vector leaked into active cache: %v", got)
	}

	// A write racing with the backfill becomes part of the activation gate.
	if err := store.Save(ctx, Memory{ID: "m2", Content: "delta revision", ContentHash: contentHash("delta revision"), Category: "fact", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, genB.ID); !errors.Is(err, ErrEmbeddingGenerationIncomplete) {
		t.Fatalf("delta should block activation: %v", err)
	}
	delta, _ := store.RevisionAt(ctx, "m2", TemporalQueryOptions{})
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: delta.RevisionID, MemoryID: "m2", GenerationID: genB.ID,
		SemanticSpaceID: genB.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identityB, ContentHash: delta.Memory.ContentHash, Vector: []float32{0.5, 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, genB.ID); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ActiveEmbeddingGeneration(ctx)
	old, _ := store.EmbeddingGeneration(ctx, genA.ID)
	if active == nil || active.ID != genB.ID || old == nil || old.State != EmbeddingGenerationRetired {
		t.Fatalf("active=%+v old=%+v", active, old)
	}
	if got, _ := store.VectorCache().Get("m1"); got[1] != 1 {
		t.Fatalf("activation did not atomically replace cache: %v", got)
	}
	// A late result from a worker that started before retirement remains
	// attributable to its generation but cannot overwrite the active
	// compatibility projection or in-memory cache.
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: current.RevisionID, MemoryID: "m1", GenerationID: genA.ID,
		SemanticSpaceID: genA.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identityA, ContentHash: current.Memory.ContentHash, Vector: []float32{-1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.VectorCache().Get("m1"); got[1] != 1 {
		t.Fatalf("retired generation result contaminated active cache: %v", got)
	}
}

func TestLegacyUnidentifiedVectorExcludedAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Memory{ID: "legacy", Content: "legacy vector", Category: "fact", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateEmbedding(ctx, "legacy", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.VectorCache().Len() != 0 {
		t.Fatal("unidentified legacy vectors must not enter semantic search")
	}
	memory, err := reopened.Get(ctx, "legacy")
	if err != nil || memory == nil || len(memory.Embedding) != 2 {
		t.Fatalf("legacy vector should remain readable as stored data: memory=%+v err=%v", memory, err)
	}
}

func TestLegacyEmbeddingAPIsCannotContaminateActiveGenerationInProcess(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "legacy-active.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(ctx, Memory{
		ID: "m1", Content: "identified content", ContentHash: contentHash("identified content"),
		Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.RevisionAt(ctx, "m1", TemporalQueryOptions{})
	if err != nil || current == nil {
		t.Fatalf("current = (%#v, %v)", current, err)
	}
	identity := EmbeddingIdentity{
		Provider: "test", Model: "identified", ModelRevision: "1",
		Dimensions: 2, Normalization: "l2",
	}
	generation, err := store.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: current.RevisionID, MemoryID: "m1", GenerationID: generation.ID,
		SemanticSpaceID: generation.SemanticSpaceID, Purpose: EmbeddingPurposeDocument,
		Identity: identity, ContentHash: current.Memory.ContentHash,
		Vector: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateEmbeddingGeneration(ctx, generation.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateEmbedding(ctx, "m1", []float32{-1, -1}); err != nil {
		t.Fatal(err)
	}
	if got, ok := store.VectorCache().Get("m1"); !ok || got[0] != 1 || got[1] != 0 {
		t.Fatalf("legacy UpdateEmbedding contaminated active cache: vector=%v ok=%v", got, ok)
	}

	if _, err := store.SaveTemporal(ctx, Memory{
		ID: "m1", Content: "new unidentified content",
		ContentHash: contentHash("new unidentified content"),
		Category:    "fact", Source: "test", Embedding: []float32{-2, -2},
	}, TemporalWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.VectorCache().Get("m1"); ok {
		t.Fatal("unidentified SaveTemporal vector entered active generation cache")
	}
	m, err := store.Get(ctx, "m1")
	if err != nil || m == nil || len(m.Embedding) != 2 {
		t.Fatalf("legacy vector should remain readable as stored data: memory=%+v err=%v", m, err)
	}
}

func TestEmbeddingGenerationReconcileDoesNotReviveFailedJob(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "failed-generation.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(ctx, Memory{
		ID: "poison", Content: "poison embedding input",
		ContentHash: contentHash("poison embedding input"), Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Memory{
		ID: "healthy", Content: "healthy embedding input",
		ContentHash: contentHash("healthy embedding input"), Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	identity := EmbeddingIdentity{Provider: "test", Model: "poison", Dimensions: 2, Normalization: "l2"}
	generation, err := store.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureEmbeddingGenerationJobs(ctx, generation.ID, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job, err := store.ClaimProcessingJob(ctx, embeddingJobKind, "worker", now, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim=%+v err=%v", job, err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE memory_processing_jobs SET state = 'failed', attempts = max_attempts,
			owner = NULL, lease_until = NULL WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureEmbeddingGenerationJobs(ctx, generation.ID, 1); err != nil {
		t.Fatal(err)
	}
	var state ProcessingJobState
	var attempts, maxAttempts int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT state, attempts, max_attempts FROM memory_processing_jobs WHERE id = ?`, job.ID,
	).Scan(&state, &attempts, &maxAttempts); err != nil {
		t.Fatal(err)
	}
	if state != JobFailed || attempts != maxAttempts {
		t.Fatalf("failed job was implicitly revived: state=%s attempts=%d/%d", state, attempts, maxAttempts)
	}
	var healthyJobs int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memory_processing_jobs
		WHERE memory_id = 'healthy' AND generation = ? AND state = 'pending'`,
		generation.ID).Scan(&healthyJobs); err != nil {
		t.Fatal(err)
	}
	if healthyJobs != 1 {
		t.Fatalf("terminal poison job starved later reconciliation: healthy jobs=%d", healthyJobs)
	}
}

func TestConcurrentEmbeddingActivationKeepsDBAndCacheOnSameGeneration(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "activation-race.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	content := "activation race source"
	if err := store.Save(ctx, Memory{
		ID: "race", Content: content, ContentHash: contentHash(content),
		Category: "fact", Source: "test",
	}); err != nil {
		t.Fatal(err)
	}
	revision, _ := store.RevisionAt(ctx, "race", TemporalQueryOptions{})
	identities := []EmbeddingIdentity{
		{Provider: "test", Model: "a", Dimensions: 2, Normalization: "l2"},
		{Provider: "test", Model: "b", Dimensions: 2, Normalization: "l2"},
	}
	generations := make([]*EmbeddingGeneration, len(identities))
	vectors := [][]float32{{1, 0}, {0, 1}}
	for i := range identities {
		generations[i], err = store.EnsureEmbeddingGeneration(ctx, identities[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
			RevisionID: revision.RevisionID, MemoryID: "race",
			GenerationID: generations[i].ID, SemanticSpaceID: generations[i].SemanticSpaceID,
			Purpose: EmbeddingPurposeDocument, Identity: identities[i],
			ContentHash: revision.Memory.ContentHash, Vector: vectors[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
	var wait sync.WaitGroup
	errorsCh := make(chan error, len(generations))
	for _, generation := range generations {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			errorsCh <- store.ActivateEmbeddingGeneration(ctx, id)
		}(generation.ID)
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	active, err := store.ActiveEmbeddingGeneration(ctx)
	if err != nil || active == nil {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	activeIndex := 0
	if active.ID == generations[1].ID {
		activeIndex = 1
	} else if active.ID != generations[0].ID {
		t.Fatalf("unexpected active generation %+v", active)
	}
	cached, ok := store.VectorCache().Get("race")
	if !ok || cached[0] != vectors[activeIndex][0] || cached[1] != vectors[activeIndex][1] {
		t.Fatalf("active generation %s cache=%v", active.ID, cached)
	}

	retiredIndex := 1 - activeIndex
	late := append([]float32(nil), vectors[retiredIndex]...)
	late[0], late[1] = 0.25, 0.75
	if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID: revision.RevisionID, MemoryID: "race",
		GenerationID:    generations[retiredIndex].ID,
		SemanticSpaceID: generations[retiredIndex].SemanticSpaceID,
		Purpose:         EmbeddingPurposeDocument, Identity: identities[retiredIndex],
		ContentHash: revision.Memory.ContentHash, Vector: late,
	}); err != nil {
		t.Fatal(err)
	}
	afterLate, _ := store.VectorCache().Get("race")
	if afterLate[0] != vectors[activeIndex][0] || afterLate[1] != vectors[activeIndex][1] {
		t.Fatalf("late retired put contaminated active cache: %v", afterLate)
	}
}
