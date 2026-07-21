package memory

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ensureCurrentEmbeddingGeneration binds the configured provider to one
// explicitly identified semantic space. It is safe to call repeatedly: the
// manifest and its snapshot/delta jobs are idempotent.
func (s *Service) ensureCurrentEmbeddingGeneration(ctx context.Context) error {
	if s.embedder == nil {
		return nil
	}
	generations, ok := s.store.(EmbeddingGenerationStore)
	if !ok {
		return nil
	}
	identity, err := EmbeddingIdentityOf(s.embedder)
	if err != nil {
		return err
	}
	generation, err := generations.EnsureEmbeddingGeneration(ctx, identity)
	if err != nil {
		return err
	}
	if generation == nil {
		return fmt.Errorf("embedding generation was not created")
	}

	s.embeddingMu.Lock()
	s.embeddingID = identity
	s.embeddingGenID = generation.ID
	s.embeddingMu.Unlock()

	active, err := generations.ActiveEmbeddingGeneration(ctx)
	if err != nil {
		return err
	}
	if active != nil && active.Identity.Compatible(identity) {
		if err := s.enableEmbeddingGeneration(ctx, generations, active); err != nil {
			return err
		}
	} else {
		// NewSQLiteStore may have loaded an active generation for a different
		// provider/model. Clear it before the current provider can issue a query.
		if active != nil {
			s.logger.Warn("embedding generation mismatch; semantic search disabled until backfill",
				"active_generation", active.ID,
				"active_model", active.Identity.Model,
				"active_dimensions", active.Identity.Dimensions,
				"building_generation", generation.ID,
				"configured_model", identity.Model,
				"configured_dimensions", identity.Dimensions,
			)
		} else {
			s.logger.Info("embedding generation building; semantic search temporarily disabled",
				"generation", generation.ID,
				"model", identity.Model,
				"dimensions", identity.Dimensions,
			)
		}
		if cache := s.store.VectorCache(); cache != nil {
			cache.Replace(nil)
		}
		if s.searcher != nil {
			s.searcher.UseEmbeddingGeneration(nil)
		}
	}

	if _, err := generations.EnsureEmbeddingGenerationJobs(ctx, generation.ID, 0); err != nil {
		return err
	}
	_, err = s.tryActivateEmbeddingGeneration(ctx, generations, generation.ID)
	return err
}

func (s *Service) currentEmbeddingGenerationID() string {
	s.embeddingMu.RLock()
	generationID := s.embeddingGenID
	s.embeddingMu.RUnlock()
	return generationID
}

func (s *Service) currentEmbeddingIdentity() (EmbeddingIdentity, bool) {
	s.embeddingMu.RLock()
	identity := s.embeddingID
	generationID := s.embeddingGenID
	s.embeddingMu.RUnlock()
	return identity, generationID != ""
}

func (s *Service) enableEmbeddingGeneration(
	ctx context.Context,
	generations EmbeddingGenerationStore,
	generation *EmbeddingGeneration,
) error {
	if generation == nil {
		return fmt.Errorf("active embedding generation is nil")
	}
	identity, ok := s.currentEmbeddingIdentity()
	if !ok || !generation.Identity.Compatible(identity) {
		return fmt.Errorf("active embedding generation is incompatible with configured provider")
	}
	if publisher, ok := s.store.(EmbeddingGenerationPublicationStore); ok && s.searcher != nil {
		return s.searcher.publishEmbeddingGeneration(generation.Identity,
			func(publish func(map[string][]float32) error) error {
				return publisher.PublishEmbeddingGeneration(
					ctx, generation.ID, false, publish,
				)
			})
	}
	vectors, err := generations.LoadEmbeddingGeneration(ctx, generation.ID)
	if err != nil {
		return err
	}
	if cache := s.store.VectorCache(); cache != nil {
		cache.Replace(vectors)
	}
	if s.searcher != nil {
		s.searcher.UseEmbeddingGeneration(&generation.Identity)
	}
	return nil
}

func (s *Service) tryActivateEmbeddingGeneration(
	ctx context.Context,
	generations EmbeddingGenerationStore,
	generationID string,
) (bool, error) {
	generation, err := generations.EmbeddingGeneration(ctx, generationID)
	if err != nil || generation == nil {
		return false, err
	}
	if generation.State == EmbeddingGenerationActive {
		return true, s.enableEmbeddingGeneration(ctx, generations, generation)
	}
	if generation.State != EmbeddingGenerationBuilding {
		return false, nil
	}
	if _, err := generations.EnsureEmbeddingGenerationJobs(ctx, generationID, 0); err != nil {
		return false, err
	}
	if publisher, ok := s.store.(EmbeddingGenerationPublicationStore); ok && s.searcher != nil {
		err := s.searcher.publishEmbeddingGeneration(generation.Identity,
			func(publish func(map[string][]float32) error) error {
				return publisher.PublishEmbeddingGeneration(
					ctx, generationID, true, publish,
				)
			})
		if errors.Is(err, ErrEmbeddingGenerationIncomplete) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if err := generations.ActivateEmbeddingGeneration(ctx, generationID); err != nil {
		if errors.Is(err, ErrEmbeddingGenerationIncomplete) {
			return false, nil
		}
		return false, err
	}
	generation, err = generations.EmbeddingGeneration(ctx, generationID)
	if err != nil {
		return false, err
	}
	if err := s.enableEmbeddingGeneration(ctx, generations, generation); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) embedRevisionForGeneration(
	ctx context.Context,
	generations EmbeddingGenerationStore,
	generation *EmbeddingGeneration,
	revision *MemoryRevision,
) error {
	if generation == nil || revision == nil {
		return fmt.Errorf("embedding generation and revision are required")
	}
	identity, ok := s.currentEmbeddingIdentity()
	if !ok || !generation.Identity.Compatible(identity) {
		return fmt.Errorf("embedding generation %q is incompatible with configured provider", generation.ID)
	}
	vectors, err := EmbedForPurpose(
		ctx, s.embedder, EmbeddingPurposeDocument,
		[]string{revision.Memory.Content},
	)
	if err != nil {
		return err
	}
	if len(vectors) != 1 {
		return fmt.Errorf("embedding provider returned %d vectors, want 1", len(vectors))
	}
	if len(vectors[0]) != generation.Identity.Dimensions {
		return fmt.Errorf("%w: got %d, want %d",
			ErrEmbeddingDimensionMismatch, len(vectors[0]), generation.Identity.Dimensions)
	}
	return generations.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID:      revision.RevisionID,
		MemoryID:        revision.MemoryID,
		GenerationID:    generation.ID,
		SemanticSpaceID: generation.SemanticSpaceID,
		Purpose:         EmbeddingPurposeDocument,
		Identity:        generation.Identity,
		ContentHash:     revision.Memory.ContentHash,
		Vector:          vectors[0],
		EmbeddedAt:      time.Now().UTC(),
	})
}

func (s *Service) backfillEmbeddingGeneration(
	ctx context.Context,
	generations EmbeddingGenerationStore,
	batchSize int,
	pause time.Duration,
	maxTotal int,
) (int, error) {
	if err := s.ensureCurrentEmbeddingGeneration(ctx); err != nil {
		return 0, err
	}
	generationID := s.currentEmbeddingGenerationID()
	if generationID == "" {
		return 0, fmt.Errorf("embedding generation is unavailable")
	}
	generation, err := generations.EmbeddingGeneration(ctx, generationID)
	if err != nil || generation == nil {
		return 0, err
	}

	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if maxTotal > 0 && total >= maxTotal {
			break
		}
		limit := batchSize
		if maxTotal > 0 && maxTotal-total < limit {
			limit = maxTotal - total
		}
		revisions, err := generations.ListMissingEmbeddingRevisions(ctx, generationID, limit)
		if err != nil {
			return total, fmt.Errorf("list pending generation embeddings: %w", err)
		}
		if len(revisions) == 0 {
			break
		}
		progress := 0
		for i := range revisions {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			if err := s.embedRevisionForGeneration(ctx, generations, generation, &revisions[i]); err != nil {
				s.logger.Warn("generation backfill embedding failed",
					"revision_id", revisions[i].RevisionID, "error", err)
				continue
			}
			progress++
			total++
		}
		if progress == 0 {
			break
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	if _, err := s.tryActivateEmbeddingGeneration(ctx, generations, generationID); err != nil {
		return total, err
	}
	s.signalDurableWorkers()
	return total, nil
}
