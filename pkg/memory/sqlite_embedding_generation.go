package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const embeddingGenerationSelect = `SELECT
	generation_id, semantic_space_id, provider, model, model_revision,
	dimensions, normalization, state, snapshot_at, created_at,
	activated_at, retired_at
	FROM embedding_generations`

func (s *SQLiteStore) EnsureEmbeddingGeneration(ctx context.Context, identity EmbeddingIdentity) (*EmbeddingGeneration, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	now := s.nowUTC()
	spaceID := identity.SemanticSpaceID()
	generationID := "gen_" + spaceID[3:]
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_generations (
		generation_id, semantic_space_id, provider, model, model_revision,
		dimensions, normalization, state, snapshot_at, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'building', ?, ?)`,
		generationID, spaceID, identity.Provider, identity.Model, identity.ModelRevision,
		identity.Dimensions, identity.Normalization, now.UnixNano(), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("ensure embedding generation: %w", err)
	}
	generation, err := s.EmbeddingGeneration(ctx, generationID)
	if err != nil || generation == nil || generation.State != EmbeddingGenerationRetired {
		return generation, err
	}
	// A semantic space can become current again after another model was active.
	// Rebuild only the missing deltas, while keeping its already identified
	// vectors, before making it eligible for activation again.
	if _, err := s.db.ExecContext(ctx, `UPDATE embedding_generations
		SET state = 'building', snapshot_at = ?, activated_at = NULL, retired_at = NULL
		WHERE generation_id = ? AND state = 'retired'`,
		now.UnixNano(), generationID); err != nil {
		return nil, fmt.Errorf("rebuild retired embedding generation: %w", err)
	}
	return s.EmbeddingGeneration(ctx, generationID)
}

func (s *SQLiteStore) ActiveEmbeddingGeneration(ctx context.Context) (*EmbeddingGeneration, error) {
	return scanEmbeddingGeneration(s.db.QueryRowContext(ctx,
		embeddingGenerationSelect+" WHERE state = 'active' LIMIT 1"))
}

func (s *SQLiteStore) EmbeddingGeneration(ctx context.Context, generationID string) (*EmbeddingGeneration, error) {
	return scanEmbeddingGeneration(s.db.QueryRowContext(ctx,
		embeddingGenerationSelect+" WHERE generation_id = ?", generationID))
}

func (s *SQLiteStore) ListBuildingEmbeddingGenerations(ctx context.Context) ([]EmbeddingGeneration, error) {
	rows, err := s.db.QueryContext(ctx,
		embeddingGenerationSelect+" WHERE state = 'building' ORDER BY created_at, generation_id")
	if err != nil {
		return nil, fmt.Errorf("list building embedding generations: %w", err)
	}
	defer rows.Close()
	var generations []EmbeddingGeneration
	for rows.Next() {
		generation, err := scanEmbeddingGenerationValue(rows)
		if err != nil {
			return nil, err
		}
		generations = append(generations, *generation)
	}
	return generations, rows.Err()
}

func (s *SQLiteStore) PutEmbeddingVector(ctx context.Context, record EmbeddingVectorRecord) error {
	s.embeddingGenerationMu.Lock()
	defer s.embeddingGenerationMu.Unlock()
	if record.EmbeddedAt.IsZero() {
		record.EmbeddedAt = s.nowUTC()
	}
	if record.SemanticSpaceID == "" {
		record.SemanticSpaceID = record.Identity.SemanticSpaceID()
	}
	if err := record.Validate(); err != nil {
		return err
	}
	generation, err := s.EmbeddingGeneration(ctx, record.GenerationID)
	if err != nil {
		return err
	}
	if generation == nil {
		return fmt.Errorf("embedding generation %q not found", record.GenerationID)
	}
	if !generation.Identity.Compatible(record.Identity) ||
		generation.SemanticSpaceID != record.SemanticSpaceID {
		return fmt.Errorf("embedding vector identity does not match generation")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin embedding vector: %w", err)
	}
	defer tx.Rollback()
	var currentState EmbeddingGenerationState
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM embedding_generations WHERE generation_id = ?`,
		record.GenerationID).Scan(&currentState); err != nil {
		return fmt.Errorf("load embedding generation state: %w", err)
	}
	var storedMemoryID string
	var storedHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT memory_id, content_hash FROM memory_revisions WHERE revision_id = ?`,
		record.RevisionID).Scan(&storedMemoryID, &storedHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("embedding revision %q not found", record.RevisionID)
		}
		return fmt.Errorf("load embedding revision: %w", err)
	}
	if storedMemoryID != record.MemoryID || storedHash.String != record.ContentHash {
		return fmt.Errorf("embedding vector source does not match immutable revision")
	}
	// The check above already pins record.ContentHash to the revision's own
	// hash, so the incoming stamp is authoritative: an existing row carrying a
	// different one is stale and must be overwritten. Gating the upsert on the
	// stored hash instead would make a stale stamp permanent — the write is
	// skipped, no error is raised, and the generation can never reach the
	// coverage its activation check demands.
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_embedding_vectors (
		revision_id, memory_id, generation_id, semantic_space_id, purpose,
		provider, model, model_revision, dimensions, normalization,
		content_hash, embedding, embedded_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(revision_id, generation_id, purpose) DO UPDATE SET
		embedding = excluded.embedding,
		embedded_at = excluded.embedded_at,
		content_hash = excluded.content_hash
	WHERE memory_embedding_vectors.semantic_space_id = excluded.semantic_space_id`,
		record.RevisionID, record.MemoryID, record.GenerationID, record.SemanticSpaceID,
		record.Purpose, record.Identity.Provider, record.Identity.Model,
		record.Identity.ModelRevision, record.Identity.Dimensions,
		record.Identity.Normalization, record.ContentHash,
		float32sToBlob(record.Vector), record.EmbeddedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store embedding vector: %w", err)
	}
	// The remaining semantic_space_id guard can still drop the upsert. Reporting
	// that as success is the exact failure this function was fixed to stop: the
	// coverage check would never be satisfied, the generation would never leave
	// 'building', and the worker would re-embed the same revision forever with
	// nothing in the logs. A dropped write is an error.
	if n, raErr := result.RowsAffected(); raErr == nil && n == 0 {
		return fmt.Errorf(
			"embedding vector write dropped for revision %s: stored semantic space differs from %s",
			record.RevisionID, record.SemanticSpaceID)
	}
	projected := false
	if currentState == EmbeddingGenerationActive && record.Purpose == EmbeddingPurposeDocument {
		result, updateErr := tx.ExecContext(ctx, `UPDATE memories SET embedding = ?
			WHERE id = ? AND current_revision_id = ? AND deleted_at IS NULL`,
			float32sToBlob(record.Vector), record.MemoryID, record.RevisionID)
		if updateErr != nil {
			return fmt.Errorf("project active embedding: %w", updateErr)
		}
		n, _ := result.RowsAffected()
		projected = n > 0
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit embedding vector: %w", err)
	}
	if projected {
		s.cache.Put(record.MemoryID, record.Vector)
	}
	return nil
}

func (s *SQLiteStore) ListMissingEmbeddingRevisions(ctx context.Context, generationID string, limit int) ([]MemoryRevision, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, revisionSelectSQL+`
		JOIN memories m ON m.current_revision_id = r.revision_id
		LEFT JOIN memory_embedding_vectors v
		  ON v.revision_id = r.revision_id
		 AND v.generation_id = ?
		 AND v.purpose = 'document'
		 AND v.content_hash = r.content_hash
		WHERE m.deleted_at IS NULL
		  AND r.is_tombstone = FALSE
		  AND v.revision_id IS NULL
		ORDER BY r.system_from, r.revision_id
		LIMIT ?`, generationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list missing embedding revisions: %w", err)
	}
	defer rows.Close()
	var revisions []MemoryRevision
	for rows.Next() {
		revision, err := scanRevisionRows(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, *revision)
	}
	return revisions, rows.Err()
}

func (s *SQLiteStore) CountMissingEmbeddingRevisions(ctx context.Context, generationID string) (int, error) {
	var missing int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories m
		JOIN memory_revisions r ON r.revision_id = m.current_revision_id
		WHERE m.deleted_at IS NULL
		  AND r.is_tombstone = FALSE
		  AND NOT EXISTS (
			SELECT 1 FROM memory_embedding_vectors v
			WHERE v.revision_id = r.revision_id
			  AND v.generation_id = ?
			  AND v.purpose = 'document'
			  AND v.content_hash = r.content_hash
		  )`, generationID).Scan(&missing)
	if err != nil {
		return 0, fmt.Errorf("count missing embedding revisions: %w", err)
	}
	return missing, nil
}

// EnsureEmbeddingGenerationJobs reconciles snapshot and delta work. The
// revision/generation uniqueness constraint makes it safe to call at startup,
// after every write, and after a worker interruption.
func (s *SQLiteStore) EnsureEmbeddingGenerationJobs(ctx context.Context, generationID string, limit int) (int, error) {
	generation, err := s.EmbeddingGeneration(ctx, generationID)
	if err != nil {
		return 0, err
	}
	if generation == nil {
		return 0, fmt.Errorf("embedding generation %q not found", generationID)
	}
	if generation.State != EmbeddingGenerationBuilding && generation.State != EmbeddingGenerationActive {
		return 0, nil
	}
	revisions, err := s.listRevisionsNeedingEmbeddingJob(ctx, generationID, limit)
	if err != nil || len(revisions) == 0 {
		return 0, err
	}
	now := s.nowUTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin reconcile embedding jobs: %w", err)
	}
	defer tx.Rollback()
	enqueued := 0
	for _, revision := range revisions {
		result, err := tx.ExecContext(ctx, `INSERT INTO memory_processing_jobs (
			id, revision_id, memory_id, kind, generation, state, attempts,
			max_attempts, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'pending', 0, 5, ?, ?)
		ON CONFLICT(revision_id, kind, generation) DO UPDATE SET
			state = 'pending', attempts = 0, owner = NULL, lease_until = NULL,
			next_attempt_at = NULL, last_error = NULL, completed_at = NULL,
			updated_at = excluded.updated_at
		WHERE memory_processing_jobs.state = 'done'`,
			newUUID(), revision.RevisionID, revision.MemoryID, embeddingJobKind,
			generationID, now.UnixNano(), now.UnixNano())
		if err != nil {
			return 0, fmt.Errorf("reconcile embedding job: %w", err)
		}
		changed, _ := result.RowsAffected()
		if changed > 0 {
			enqueued++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit embedding job reconciliation: %w", err)
	}
	return enqueued, nil
}

func (s *SQLiteStore) listRevisionsNeedingEmbeddingJob(
	ctx context.Context,
	generationID string,
	limit int,
) ([]MemoryRevision, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, revisionSelectSQL+`
		JOIN memories m ON m.current_revision_id = r.revision_id
		WHERE m.deleted_at IS NULL
		  AND r.is_tombstone = FALSE
		  AND NOT EXISTS (
			SELECT 1 FROM memory_embedding_vectors v
			WHERE v.revision_id = r.revision_id
			  AND v.generation_id = ?
			  AND v.purpose = 'document'
			  AND v.content_hash = r.content_hash
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM memory_processing_jobs j
			WHERE j.revision_id = r.revision_id
			  AND j.kind = ?
			  AND j.generation = ?
			  AND j.state IN ('pending', 'processing', 'failed')
		  )
		ORDER BY r.system_from, r.revision_id
		LIMIT ?`, generationID, embeddingJobKind, generationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list revisions needing embedding jobs: %w", err)
	}
	defer rows.Close()
	var revisions []MemoryRevision
	for rows.Next() {
		revision, err := scanRevisionRows(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, *revision)
	}
	return revisions, rows.Err()
}

func (s *SQLiteStore) ActivateEmbeddingGeneration(ctx context.Context, generationID string) error {
	return s.PublishEmbeddingGeneration(ctx, generationID, true, func(vectors map[string][]float32) error {
		s.cache.Replace(vectors)
		return nil
	})
}

func (s *SQLiteStore) PublishEmbeddingGeneration(
	ctx context.Context,
	generationID string,
	activate bool,
	publish func(map[string][]float32) error,
) error {
	s.embeddingGenerationMu.Lock()
	defer s.embeddingGenerationMu.Unlock()
	if activate {
		if err := s.activateEmbeddingGenerationLocked(ctx, generationID); err != nil {
			return err
		}
	}
	var state EmbeddingGenerationState
	if err := s.db.QueryRowContext(ctx,
		`SELECT state FROM embedding_generations WHERE generation_id = ?`,
		generationID).Scan(&state); err != nil {
		return fmt.Errorf("load published embedding generation: %w", err)
	}
	if state != EmbeddingGenerationActive {
		return fmt.Errorf("%w: cannot publish %s generation", ErrEmbeddingGenerationTransition, state)
	}
	vectors, err := s.loadEmbeddingGeneration(ctx, generationID)
	if err != nil {
		return err
	}
	if publish == nil {
		s.cache.Replace(vectors)
		return nil
	}
	return publish(vectors)
}

func (s *SQLiteStore) activateEmbeddingGenerationLocked(ctx context.Context, generationID string) error {
	now := s.nowUTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin activate embedding generation: %w", err)
	}
	defer tx.Rollback()

	var state EmbeddingGenerationState
	if err := tx.QueryRowContext(ctx,
		`SELECT state FROM embedding_generations WHERE generation_id = ?`,
		generationID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("embedding generation %q not found", generationID)
		}
		return err
	}
	if state == EmbeddingGenerationActive {
		return nil
	}
	if state != EmbeddingGenerationBuilding {
		return fmt.Errorf("%w: %s to active", ErrEmbeddingGenerationTransition, state)
	}
	var missing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories m
		JOIN memory_revisions r ON r.revision_id = m.current_revision_id
		WHERE m.deleted_at IS NULL AND r.is_tombstone = FALSE
		  AND NOT EXISTS (
			SELECT 1 FROM memory_embedding_vectors v
			WHERE v.revision_id = r.revision_id
			  AND v.generation_id = ?
			  AND v.purpose = 'document'
			  AND v.content_hash = r.content_hash
		  )`, generationID).Scan(&missing); err != nil {
		return fmt.Errorf("check embedding generation coverage: %w", err)
	}
	if missing != 0 {
		return fmt.Errorf("%w: %d current revisions missing", ErrEmbeddingGenerationIncomplete, missing)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE embedding_generations
		SET state = 'retired', retired_at = ?
		WHERE state = 'active' AND generation_id <> ?`, now.UnixNano(), generationID); err != nil {
		return fmt.Errorf("retire embedding generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE embedding_generations
		SET state = 'active', activated_at = ?, retired_at = NULL
		WHERE generation_id = ? AND state = 'building'`, now.UnixNano(), generationID); err != nil {
		return fmt.Errorf("activate embedding generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET embedding = (
		SELECT v.embedding FROM memory_embedding_vectors v
		WHERE v.revision_id = memories.current_revision_id
		  AND v.generation_id = ?
		  AND v.purpose = 'document'
	) WHERE deleted_at IS NULL`, generationID); err != nil {
		return fmt.Errorf("project activated embeddings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit activate embedding generation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LoadEmbeddingGeneration(ctx context.Context, generationID string) (map[string][]float32, error) {
	return s.loadEmbeddingGeneration(ctx, generationID)
}

func (s *SQLiteStore) loadEmbeddingGeneration(ctx context.Context, generationID string) (map[string][]float32, error) {
	generation, err := s.EmbeddingGeneration(ctx, generationID)
	if err != nil {
		return nil, err
	}
	if generation == nil {
		return nil, fmt.Errorf("embedding generation %q not found", generationID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
			m.id, v.semantic_space_id, v.provider, v.model, v.model_revision,
			v.dimensions, v.normalization, v.embedding
		FROM memories m
		JOIN memory_embedding_vectors v
		  ON v.revision_id = m.current_revision_id
		 AND v.generation_id = ?
		 AND v.purpose = 'document'
		WHERE m.deleted_at IS NULL`, generationID)
	if err != nil {
		return nil, fmt.Errorf("load embedding generation: %w", err)
	}
	defer rows.Close()
	vectors := make(map[string][]float32)
	for rows.Next() {
		var id string
		var semanticSpaceID, provider, model, modelRevision, normalization string
		var dimensions int
		var blob []byte
		if err := rows.Scan(
			&id, &semanticSpaceID, &provider, &model, &modelRevision,
			&dimensions, &normalization, &blob,
		); err != nil {
			return nil, err
		}
		storedIdentity := EmbeddingIdentity{
			Provider: provider, Model: model, ModelRevision: modelRevision,
			Dimensions: dimensions, Normalization: normalization,
		}
		if semanticSpaceID != generation.SemanticSpaceID ||
			!storedIdentity.Compatible(generation.Identity) {
			return nil, fmt.Errorf("embedding vector for memory %s does not match generation %s",
				id, generationID)
		}
		vector, err := blobToFloat32s(blob)
		if err != nil {
			return nil, err
		}
		if len(vector) != generation.Identity.Dimensions {
			return nil, fmt.Errorf("%w: memory %s has %d, want %d",
				ErrEmbeddingDimensionMismatch, id, len(vector), generation.Identity.Dimensions)
		}
		vectors[id] = vector
	}
	return vectors, rows.Err()
}

func scanEmbeddingGeneration(scanner revisionScanner) (*EmbeddingGeneration, error) {
	generation, err := scanEmbeddingGenerationValue(scanner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return generation, err
}

func scanEmbeddingGenerationValue(scanner revisionScanner) (*EmbeddingGeneration, error) {
	var generation EmbeddingGeneration
	var snapshotNS, createdNS int64
	var activatedNS, retiredNS sql.NullInt64
	if err := scanner.Scan(
		&generation.ID, &generation.SemanticSpaceID,
		&generation.Identity.Provider, &generation.Identity.Model,
		&generation.Identity.ModelRevision, &generation.Identity.Dimensions,
		&generation.Identity.Normalization, &generation.State,
		&snapshotNS, &createdNS, &activatedNS, &retiredNS,
	); err != nil {
		return nil, err
	}
	generation.SnapshotAt = time.Unix(0, snapshotNS).UTC()
	generation.CreatedAt = time.Unix(0, createdNS).UTC()
	generation.ActivatedAt = nullNanoTime(activatedNS)
	generation.RetiredAt = nullNanoTime(retiredNS)
	return &generation, nil
}
