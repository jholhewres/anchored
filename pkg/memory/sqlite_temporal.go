package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var _ TemporalStore = (*SQLiteStore)(nil)
var _ TemporalMutationStore = (*SQLiteStore)(nil)

var (
	ErrTemporalIdentityConflict = errors.New("logical identity belongs to a different memory")
	ErrFutureTemporalInterval   = errors.New("future temporal boundaries require projection promotion")
)

// SaveTemporal atomically records the immutable revision and updates memories,
// which remains the backward-compatible current view.
func (s *SQLiteStore) SaveTemporal(ctx context.Context, m Memory, opts TemporalWriteOptions) (*MemoryRevision, error) {
	return s.saveTemporal(ctx, m, opts, nil)
}

// UpdateTemporal acquires the write transaction before reading the current
// memory, so concurrent partial updates cannot restore a stale snapshot.
func (s *SQLiteStore) UpdateTemporal(
	ctx context.Context,
	id string,
	mutation TemporalMutation,
	opts TemporalWriteOptions,
) (*MemoryRevision, error) {
	if id == "" {
		return nil, ErrTemporalIdentityRequired
	}
	return s.saveTemporal(ctx, Memory{ID: id}, opts, &mutation)
}

// updateMetadataTemporal records a system-time correction without changing
// the compatibility row's updated_at, content, keywords, embedding, or FTS
// index. Historically UpdateMetadata was metadata-only; the temporal ledger
// must not turn it into a content update.
func (s *SQLiteStore) updateMetadataTemporal(ctx context.Context, id string, metadata any) error {
	if id == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin metadata correction: %w", err)
	}
	defer tx.Rollback()

	current, logicalID, currentRevisionID, err := loadCurrentMemoryTx(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if logicalID == "" {
		logicalID = current.ID
	}
	now, err := advanceSystemTimeTx(ctx, tx, logicalID, s.nowUTC())
	if err != nil {
		return err
	}
	validFrom, validTo, err := resolveWriteIntervalTx(
		ctx, tx, TemporalCorrect, currentRevisionID, TemporalWriteOptions{}, now,
	)
	if err != nil {
		return err
	}
	if err := replaceCurrentIntervalTx(ctx, tx, logicalID, validFrom, validTo, now); err != nil {
		return err
	}

	current.Metadata = metadata
	revision := MemoryRevision{
		RevisionID: newUUID(), MemoryID: current.ID, LogicalID: logicalID,
		Memory: *current, Mode: TemporalCorrect,
		ValidFrom: validFrom, ValidTo: cloneTime(validTo), SystemFrom: now,
	}
	if err := insertRevisionTx(ctx, tx, revision); err != nil {
		return err
	}
	if err := copyEmbeddingVectorsToRevisionTx(
		ctx, tx, currentRevisionID, revision.RevisionID, current.ID,
	); err != nil {
		return err
	}
	if err := validateBitemporalIntervalsTx(ctx, tx, logicalID); err != nil {
		return err
	}
	var encodedMetadata any
	if metadata != nil {
		value, marshalErr := json.Marshal(metadata)
		if marshalErr != nil {
			return fmt.Errorf("marshal metadata: %w", marshalErr)
		}
		encodedMetadata = string(value)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE memories
		 SET metadata = ?, logical_id = ?, current_revision_id = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		encodedMetadata, logicalID, revision.RevisionID, id,
	)
	if err != nil {
		return fmt.Errorf("update current metadata view: %w", err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("inspect metadata correction: %w", rowsErr)
	} else if changed != 1 {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit metadata correction: %w", err)
	}
	return nil
}

// copyEmbeddingVectorsToRevisionTx reuses immutable vectors for a revision
// whose content did not change. Metadata-only corrections must remain cheap
// and must not temporarily remove a memory from semantic search.
func copyEmbeddingVectorsToRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	fromRevisionID, toRevisionID, memoryID string,
) error {
	if fromRevisionID == "" || toRevisionID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO memory_embedding_vectors (
			revision_id, memory_id, generation_id, semantic_space_id, purpose,
			provider, model, model_revision, dimensions, normalization,
			content_hash, embedding, embedded_at
		)
		SELECT ?, ?, generation_id, semantic_space_id, purpose,
			provider, model, model_revision, dimensions, normalization,
			content_hash, embedding, embedded_at
		FROM memory_embedding_vectors
		WHERE revision_id = ?
		ON CONFLICT(revision_id, generation_id, purpose) DO NOTHING`,
		toRevisionID, memoryID, fromRevisionID,
	)
	if err != nil {
		return fmt.Errorf("copy metadata-stable embedding vectors: %w", err)
	}
	return nil
}

func (s *SQLiteStore) softDeleteByScopeTemporal(
	ctx context.Context,
	where string,
	args []any,
) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin soft delete by scope: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(
		ctx, "SELECT id FROM memories WHERE deleted_at IS NULL AND "+where, args...,
	)
	if err != nil {
		return 0, fmt.Errorf("list soft-delete scope: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		if err := s.softDeleteMemoryTx(ctx, tx, id); err != nil {
			return 0, fmt.Errorf("soft delete %s by scope: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit soft delete by scope: %w", err)
	}
	for _, id := range ids {
		s.cache.Remove(id)
	}
	return len(ids), nil
}

func (s *SQLiteStore) softDeleteMemoryTx(ctx context.Context, tx *sql.Tx, id string) error {
	current, logicalID, currentRevisionID, err := loadCurrentMemoryTx(ctx, tx, id, "")
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	tombstone, err := revisionIsTombstoneTx(ctx, tx, currentRevisionID)
	if err != nil {
		return err
	}
	if tombstone {
		return nil
	}
	if logicalID == "" {
		logicalID = current.ID
	}
	now, err := advanceSystemTimeTx(ctx, tx, logicalID, s.nowUTC())
	if err != nil {
		return err
	}
	current.UpdatedAt = now
	if err := replaceCurrentIntervalTx(ctx, tx, logicalID, now, nil, now); err != nil {
		return err
	}
	revision := MemoryRevision{
		RevisionID: newUUID(), MemoryID: current.ID, LogicalID: logicalID,
		Memory: *current, Mode: TemporalTombstone, IsTombstone: true,
		ValidFrom: now, SystemFrom: now,
	}
	if err := insertRevisionTx(ctx, tx, revision); err != nil {
		return err
	}
	if err := cancelRemoteOutboxForMemoryTx(ctx, tx, current.ID, now); err != nil {
		return err
	}
	if err := validateBitemporalIntervalsTx(ctx, tx, logicalID); err != nil {
		return err
	}
	if err := upsertCurrentMemoryTx(
		ctx, tx, *current, logicalID, revision.RevisionID, true, now,
	); err != nil {
		return err
	}
	return nil
}

func (s *SQLiteStore) saveTemporal(
	ctx context.Context,
	m Memory,
	opts TemporalWriteOptions,
	mutation *TemporalMutation,
) (*MemoryRevision, error) {
	mode, err := normalizeTemporalMode(opts.Mode)
	if err != nil {
		return nil, err
	}
	explicitIdentity := opts.LogicalID != "" || m.ID != ""
	if (mode == TemporalCorrect || mode == TemporalTombstone) && !explicitIdentity {
		return nil, ErrTemporalIdentityRequired
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin temporal save: %w", err)
	}
	defer tx.Rollback()

	now := s.nowUTC()
	existing, existingLogicalID, currentRevisionID, err := resolveTemporalIdentityTx(
		ctx, tx, m.ID, opts.LogicalID,
	)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if mutation != nil {
			currentTombstone, stateErr := revisionIsTombstoneTx(ctx, tx, currentRevisionID)
			if stateErr != nil {
				return nil, stateErr
			}
			switch mutation.ExpectedState {
			case TemporalStateActive:
				if currentTombstone {
					return nil, nil
				}
			case TemporalStateTombstone:
				if !currentTombstone {
					return nil, nil
				}
			case TemporalStateAny:
			default:
				return nil, fmt.Errorf("unknown temporal mutation state %q", mutation.ExpectedState)
			}
			m = *existing
			applyTemporalMutation(&m, *mutation)
		}
		if m.ID == "" {
			m.ID = existing.ID
		}
		if opts.LogicalID != "" && existingLogicalID != "" && opts.LogicalID != existingLogicalID {
			return nil, fmt.Errorf("logical identity mismatch: memory %s belongs to %s", m.ID, existingLogicalID)
		}
		if m.CreatedAt.IsZero() {
			m.CreatedAt = existing.CreatedAt
		}
	} else if mutation != nil {
		// Preserve Store.Update/UpdateMetadata's legacy no-op behavior for a
		// missing memory.
		return nil, nil
	}
	if m.ID == "" {
		m.ID = newUUID()
	}

	logicalID := opts.LogicalID
	if logicalID == "" {
		logicalID = existingLogicalID
	}
	if logicalID == "" {
		logicalID = m.ID
	}
	if (mode == TemporalCorrect || mode == TemporalTombstone) && existing == nil {
		return nil, fmt.Errorf("%w: %s", ErrTemporalIdentityRequired, logicalID)
	}

	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.ContentHash == "" && m.Content != "" {
		m.ContentHash = contentHash(m.Content)
	}

	// System time must advance even under a deterministic/coarse test clock or
	// two writes in the same nanosecond, otherwise half-open intervals collapse.
	now, err = advanceSystemTimeTx(ctx, tx, logicalID, now)
	if err != nil {
		return nil, err
	}
	m.UpdatedAt = now

	validFrom, validTo, err := resolveWriteIntervalTx(ctx, tx, mode, currentRevisionID, opts, now)
	if err != nil {
		return nil, err
	}
	if err := validTemporalInterval(validFrom, validTo); err != nil {
		return nil, err
	}
	if validFrom.After(now) || temporalBoundaryIsFuture(validTo, now) {
		return nil, ErrFutureTemporalInterval
	}

	if existing != nil {
		if err := replaceCurrentIntervalTx(ctx, tx, logicalID, validFrom, validTo, now); err != nil {
			return nil, err
		}
	}

	isTombstone := mode == TemporalTombstone
	if mode == TemporalCorrect {
		isTombstone, err = revisionIsTombstoneTx(ctx, tx, currentRevisionID)
		if err != nil {
			return nil, err
		}
	}
	revision := MemoryRevision{
		RevisionID:  newUUID(),
		MemoryID:    m.ID,
		LogicalID:   logicalID,
		Memory:      m,
		Mode:        mode,
		IsTombstone: isTombstone,
		ValidFrom:   validFrom,
		ValidTo:     cloneTime(validTo),
		SystemFrom:  now,
	}
	if err := insertRevisionTx(ctx, tx, revision); err != nil {
		return nil, err
	}
	if err := insertProcessingJobsTx(ctx, tx, revision, opts.ProcessingJobs, now); err != nil {
		return nil, err
	}
	if err := insertRemoteOutboxTx(ctx, tx, revision, opts.RemoteOutbox, now); err != nil {
		return nil, err
	}
	if revision.IsTombstone {
		if err := cancelRemoteOutboxForMemoryTx(
			ctx, tx, revision.MemoryID, now,
		); err != nil {
			return nil, err
		}
	}
	if err := validateBitemporalIntervalsTx(ctx, tx, logicalID); err != nil {
		return nil, err
	}
	projection := revision
	if selected, selectErr := selectCurrentRevisionTx(ctx, tx, logicalID, now); selectErr != nil {
		return nil, selectErr
	} else if selected != nil {
		projection = *selected
	}
	if projection.RevisionID == revision.RevisionID {
		// The row read from memory_revisions intentionally excludes volatile
		// compatibility fields, so keep the complete write value when the new
		// revision itself is current.
		projection.Memory = m
	} else if existing != nil {
		// Historical/right-hand clones carry the immutable payload. Preserve
		// current-view-only counters and sync state from the pre-write row.
		projection.Memory.AccessCount = existing.AccessCount
		projection.Memory.LastAccessed = existing.LastAccessed
		projection.Memory.SyncDirty = existing.SyncDirty
		projection.Memory.SyncOrigin = existing.SyncOrigin
		if projection.Memory.ContentHash == existing.ContentHash {
			projection.Memory.Embedding = existing.Embedding
		}
	}
	if err := upsertCurrentMemoryTx(
		ctx, tx, projection.Memory, logicalID, projection.RevisionID,
		projection.IsTombstone, now,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit temporal save: %w", err)
	}

	s.refreshVectorCache(ctx, projection.Memory.ID)
	return &revision, nil
}

func temporalBoundaryIsFuture(boundary *time.Time, now time.Time) bool {
	return boundary != nil && boundary.UTC().After(now)
}

// resolveTemporalIdentityTx treats an explicit logical identity as canonical.
// memory_id remains the stable compatibility-row key; a caller cannot attach a
// second physical row to an existing logical history.
func resolveTemporalIdentityTx(
	ctx context.Context,
	tx *sql.Tx,
	memoryID, logicalID string,
) (*Memory, string, string, error) {
	if logicalID != "" {
		existing, storedLogicalID, revisionID, err := loadCurrentMemoryTx(
			ctx, tx, "", logicalID,
		)
		if err != nil {
			return nil, "", "", err
		}
		if existing != nil {
			if memoryID != "" && memoryID != existing.ID {
				return nil, "", "", fmt.Errorf(
					"%w: logical identity %s belongs to memory %s, not %s",
					ErrTemporalIdentityConflict, logicalID, existing.ID, memoryID,
				)
			}
			return existing, storedLogicalID, revisionID, nil
		}
	}

	existing, storedLogicalID, revisionID, err := loadCurrentMemoryTx(
		ctx, tx, memoryID, "",
	)
	if err != nil || existing == nil || logicalID == "" {
		return existing, storedLogicalID, revisionID, err
	}
	if storedLogicalID != "" && storedLogicalID != logicalID {
		return nil, "", "", fmt.Errorf(
			"%w: memory %s belongs to logical identity %s, not %s",
			ErrTemporalIdentityConflict, memoryID, storedLogicalID, logicalID,
		)
	}
	return existing, storedLogicalID, revisionID, nil
}

func selectCurrentRevisionTx(
	ctx context.Context,
	tx *sql.Tx,
	logicalID string,
	validAt time.Time,
) (*MemoryRevision, error) {
	query := revisionSelectSQL + `
		WHERE r.logical_id = ?
		  AND r.system_to IS NULL
		  AND r.valid_from <= ?
		  AND (r.valid_to IS NULL OR ? < r.valid_to)
		ORDER BY r.valid_from DESC, r.system_from DESC, r.revision_id ASC
		LIMIT 1`
	revision, err := scanRevision(tx.QueryRowContext(
		ctx, query, logicalID, validAt.UnixNano(), validAt.UnixNano(),
	))
	if err != nil {
		return nil, fmt.Errorf("select current temporal projection: %w", err)
	}
	return revision, nil
}

func applyTemporalMutation(current *Memory, mutation TemporalMutation) {
	if mutation.Content != nil {
		current.Content = *mutation.Content
		current.ContentHash = contentHash(current.Content)
		current.Keywords = ExtractKeywords(current.Content)
	}
	if mutation.Category != nil {
		current.Category = *mutation.Category
	}
	if mutation.SetMetadata {
		current.Metadata = mutation.Metadata
	}
	if mutation.ClearEmbedding {
		current.Embedding = nil
	}
}

// RevisionAt independently selects valid and system time. Omitting both axes
// resolves the revision referenced by the compatibility current row.
func (s *SQLiteStore) RevisionAt(ctx context.Context, logicalID string, opts TemporalQueryOptions) (*MemoryRevision, error) {
	if logicalID == "" {
		return nil, ErrTemporalIdentityRequired
	}
	if opts.ValidAt == nil && opts.KnownAt == nil {
		q := revisionSelectSQL + `
			JOIN memories current ON current.current_revision_id = r.revision_id
			WHERE current.logical_id = ?`
		if !opts.IncludeTombstone {
			q += " AND r.is_tombstone = FALSE"
		}
		row := s.db.QueryRowContext(ctx, q, logicalID)
		return scanRevision(row)
	}

	queryStart := s.nowUTC()
	var validAt, knownAt time.Time
	switch {
	case opts.ValidAt != nil && opts.KnownAt != nil:
		validAt = opts.ValidAt.UTC()
		knownAt = opts.KnownAt.UTC()
	case opts.ValidAt != nil:
		validAt = opts.ValidAt.UTC()
		knownAt = queryStart
	default:
		knownAt = opts.KnownAt.UTC()
		validAt = knownAt
	}

	q := revisionSelectSQL + `
		WHERE r.logical_id = ?
		  AND r.valid_from <= ?
		  AND (r.valid_to IS NULL OR ? < r.valid_to)
		  AND r.system_from <= ?
		  AND (r.system_to IS NULL OR ? < r.system_to)`
	if !opts.IncludeTombstone {
		q += " AND r.is_tombstone = FALSE"
	}
	q += " ORDER BY r.system_from DESC, r.valid_from DESC, r.revision_id ASC LIMIT 1"

	return scanRevision(s.db.QueryRowContext(ctx, q,
		logicalID, validAt.UnixNano(), validAt.UnixNano(), knownAt.UnixNano(), knownAt.UnixNano()))
}

func (s *SQLiteStore) ListRevisions(ctx context.Context, logicalID string) ([]MemoryRevision, error) {
	if logicalID == "" {
		return nil, ErrTemporalIdentityRequired
	}
	rows, err := s.db.QueryContext(ctx, revisionSelectSQL+`
		WHERE r.logical_id = ?
		ORDER BY r.system_from ASC, r.valid_from ASC, r.revision_id ASC`, logicalID)
	if err != nil {
		return nil, fmt.Errorf("list memory revisions: %w", err)
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

const revisionSelectSQL = `SELECT
	r.revision_id, r.memory_id, r.logical_id, r.project_id, r.category,
	r.content, r.content_hash, r.keywords, r.source, r.source_id, r.metadata,
	r.memory_created_at, r.memory_updated_at, r.author, r.remote_project_key,
	r.temporal_mode, r.is_tombstone, r.valid_from, r.valid_to,
	r.system_from, r.system_to
	FROM memory_revisions r `

func loadCurrentMemoryTx(ctx context.Context, tx *sql.Tx, memoryID, logicalID string) (*Memory, string, string, error) {
	if memoryID == "" && logicalID == "" {
		return nil, "", "", nil
	}
	query := `SELECT id, project_id, category, content, content_hash, keywords, embedding,
		source, source_id, created_at, updated_at, access_count, last_accessed_at,
		metadata, sync_dirty, sync_origin, author, remote_project_key,
		logical_id, current_revision_id
		FROM memories WHERE `
	var arg string
	if memoryID != "" {
		query += "id = ?"
		arg = memoryID
	} else {
		query += "logical_id = ?"
		arg = logicalID
	}

	row := tx.QueryRowContext(ctx, query, arg)
	var m Memory
	var keywords, metadata, project, source, sourceID, contentHash sql.NullString
	var syncOrigin, author, remote, storedLogical, currentRevision sql.NullString
	var embedding []byte
	var lastAccessed sql.NullTime
	if err := row.Scan(
		&m.ID, &project, &m.Category, &m.Content, &contentHash, &keywords, &embedding,
		&source, &sourceID, &m.CreatedAt, &m.UpdatedAt, &m.AccessCount, &lastAccessed,
		&metadata, &m.SyncDirty, &syncOrigin, &author, &remote,
		&storedLogical, &currentRevision,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", "", nil
		}
		return nil, "", "", fmt.Errorf("load current temporal memory: %w", err)
	}
	m.ProjectID = nilIfNull(project)
	m.ContentHash = contentHash.String
	m.Keywords = unmarshalKeywords(keywords)
	m.Source = source.String
	m.SourceID = nilIfNull(sourceID)
	m.LastAccessed = nilTimeIfZero(lastAccessed)
	m.SyncOrigin = syncOrigin.String
	m.Author = nilIfNull(author)
	m.RemoteProjectKey = nilIfNull(remote)
	if metadata.Valid {
		_ = json.Unmarshal([]byte(metadata.String), &m.Metadata)
	}
	if len(embedding) > 0 {
		m.Embedding, _ = blobToFloat32s(embedding)
	}
	return &m, storedLogical.String, currentRevision.String, nil
}

func resolveWriteIntervalTx(
	ctx context.Context,
	tx *sql.Tx,
	mode TemporalMode,
	currentRevisionID string,
	opts TemporalWriteOptions,
	now time.Time,
) (time.Time, *time.Time, error) {
	if mode != TemporalCorrect {
		start := now
		if opts.ValidFrom != nil {
			start = opts.ValidFrom.UTC()
		}
		return start, utcTime(opts.ValidTo), nil
	}

	if currentRevisionID == "" {
		return time.Time{}, nil, ErrTemporalIdentityRequired
	}
	var startNS int64
	var endNS sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT valid_from, valid_to FROM memory_revisions WHERE revision_id = ?`,
		currentRevisionID,
	).Scan(&startNS, &endNS); err != nil {
		return time.Time{}, nil, fmt.Errorf("load corrected interval: %w", err)
	}
	start := time.Unix(0, startNS).UTC()
	end := nullNanoTime(endNS)
	if opts.ValidFrom != nil && !opts.ValidFrom.UTC().Equal(start) {
		return time.Time{}, nil, ErrCorrectionIntervalChange
	}
	if opts.ValidTo != nil {
		if end == nil || !opts.ValidTo.UTC().Equal(*end) {
			return time.Time{}, nil, ErrCorrectionIntervalChange
		}
	}
	return start, end, nil
}

func revisionIsTombstoneTx(ctx context.Context, tx *sql.Tx, revisionID string) (bool, error) {
	if revisionID == "" {
		return false, ErrTemporalIdentityRequired
	}
	var tombstone bool
	if err := tx.QueryRowContext(ctx,
		`SELECT is_tombstone FROM memory_revisions WHERE revision_id = ?`,
		revisionID,
	).Scan(&tombstone); err != nil {
		return false, fmt.Errorf("load corrected revision state: %w", err)
	}
	return tombstone, nil
}

func advanceSystemTimeTx(ctx context.Context, tx *sql.Tx, logicalID string, candidate time.Time) (time.Time, error) {
	var maxNS sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(system_from) FROM memory_revisions WHERE logical_id = ?`,
		logicalID,
	).Scan(&maxNS); err != nil {
		return time.Time{}, fmt.Errorf("read latest system time: %w", err)
	}
	if maxNS.Valid && candidate.UnixNano() <= maxNS.Int64 {
		return time.Unix(0, maxNS.Int64+1).UTC(), nil
	}
	return candidate.UTC(), nil
}

func replaceCurrentIntervalTx(
	ctx context.Context,
	tx *sql.Tx,
	logicalID string,
	replacementStart time.Time,
	replacementEnd *time.Time,
	systemTime time.Time,
) error {
	q := revisionSelectSQL + `
		WHERE r.logical_id = ?
		  AND r.system_to IS NULL
		  AND (r.valid_to IS NULL OR ? < r.valid_to)
		  AND (? IS NULL OR r.valid_from < ?)
		ORDER BY r.valid_from ASC`
	var endArg any
	if replacementEnd != nil {
		endArg = replacementEnd.UnixNano()
	}
	rows, err := tx.QueryContext(ctx, q, logicalID, replacementStart.UnixNano(), endArg, endArg)
	if err != nil {
		return fmt.Errorf("select overlapping revisions: %w", err)
	}
	var overlapping []MemoryRevision
	for rows.Next() {
		revision, scanErr := scanRevisionRows(rows)
		if scanErr != nil {
			rows.Close()
			return scanErr
		}
		overlapping = append(overlapping, *revision)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, prior := range overlapping {
		result, err := tx.ExecContext(ctx,
			`UPDATE memory_revisions SET system_to = ?
			 WHERE revision_id = ? AND system_to IS NULL`,
			systemTime.UnixNano(), prior.RevisionID)
		if err != nil {
			return fmt.Errorf("close revision system interval: %w", err)
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return fmt.Errorf("close revision system interval %s: concurrent modification", prior.RevisionID)
		}

		if prior.ValidFrom.Before(replacementStart) {
			leftEnd := replacementStart
			if prior.ValidTo != nil && prior.ValidTo.Before(leftEnd) {
				leftEnd = *prior.ValidTo
			}
			if prior.ValidFrom.Before(leftEnd) {
				left := cloneRevisionSegment(prior, prior.ValidFrom, &leftEnd, systemTime)
				if err := insertRevisionTx(ctx, tx, left); err != nil {
					return err
				}
				if err := copyEmbeddingVectorsToRevisionTx(
					ctx, tx, prior.RevisionID, left.RevisionID, left.MemoryID,
				); err != nil {
					return err
				}
			}
		}
		if replacementEnd != nil && (prior.ValidTo == nil || replacementEnd.Before(*prior.ValidTo)) {
			right := cloneRevisionSegment(prior, *replacementEnd, prior.ValidTo, systemTime)
			if err := insertRevisionTx(ctx, tx, right); err != nil {
				return err
			}
			if err := copyEmbeddingVectorsToRevisionTx(
				ctx, tx, prior.RevisionID, right.RevisionID, right.MemoryID,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func cloneRevisionSegment(prior MemoryRevision, start time.Time, end *time.Time, systemTime time.Time) MemoryRevision {
	prior.RevisionID = newUUID()
	prior.ValidFrom = start
	prior.ValidTo = cloneTime(end)
	prior.SystemFrom = systemTime
	prior.SystemTo = nil
	return prior
}

func validateBitemporalIntervalsTx(ctx context.Context, tx *sql.Tx, logicalID string) error {
	var overlap int
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM memory_revisions a
			JOIN memory_revisions b
			  ON a.logical_id = b.logical_id
			 AND a.revision_id < b.revision_id
			WHERE a.logical_id = ?
			  AND (a.valid_to IS NULL OR b.valid_from < a.valid_to)
			  AND (b.valid_to IS NULL OR a.valid_from < b.valid_to)
			  AND (a.system_to IS NULL OR b.system_from < a.system_to)
			  AND (b.system_to IS NULL OR a.system_from < b.system_to)
			LIMIT 1
		)`, logicalID).Scan(&overlap)
	if err != nil {
		return fmt.Errorf("validate bitemporal intervals: %w", err)
	}
	if overlap != 0 {
		return ErrTemporalOverlap
	}
	return nil
}

func insertRevisionTx(ctx context.Context, tx *sql.Tx, revision MemoryRevision) error {
	keywords, metadata, err := encodeRevisionJSON(revision.Memory)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_revisions (
		revision_id, memory_id, logical_id, project_id, category, content,
		content_hash, keywords, source, source_id, metadata,
		memory_created_at, memory_updated_at, author, remote_project_key,
		temporal_mode, is_tombstone, valid_from, valid_to, system_from, system_to
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		revision.RevisionID, revision.MemoryID, revision.LogicalID,
		revision.Memory.ProjectID, revision.Memory.Category, revision.Memory.Content,
		revision.Memory.ContentHash, keywords, revision.Memory.Source, revision.Memory.SourceID, metadata,
		revision.Memory.CreatedAt, revision.Memory.UpdatedAt, revision.Memory.Author, revision.Memory.RemoteProjectKey,
		revision.Mode, revision.IsTombstone, revision.ValidFrom.UnixNano(), timeNano(revision.ValidTo),
		revision.SystemFrom.UnixNano(), timeNano(revision.SystemTo),
	)
	if err != nil {
		return fmt.Errorf("insert memory revision: %w", err)
	}
	return nil
}

func upsertCurrentMemoryTx(
	ctx context.Context,
	tx *sql.Tx,
	m Memory,
	logicalID, revisionID string,
	tombstone bool,
	now time.Time,
) error {
	keywords, metadata, err := encodeRevisionJSON(m)
	if err != nil {
		return err
	}
	var embedding any
	if m.Embedding != nil {
		embedding = float32sToBlob(m.Embedding)
	}
	var deletedAt any
	if tombstone {
		deletedAt = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memories (
		id, project_id, category, content, content_hash, keywords, embedding,
		source, source_id, created_at, updated_at, access_count, last_accessed_at,
		metadata, sync_dirty, sync_origin, author, remote_project_key,
		deleted_at, logical_id, current_revision_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		project_id = excluded.project_id,
		category = excluded.category,
		content = excluded.content,
		content_hash = excluded.content_hash,
		keywords = excluded.keywords,
		embedding = excluded.embedding,
		source = excluded.source,
		source_id = excluded.source_id,
		updated_at = excluded.updated_at,
		metadata = excluded.metadata,
		sync_dirty = excluded.sync_dirty,
		sync_origin = excluded.sync_origin,
		author = excluded.author,
		remote_project_key = excluded.remote_project_key,
		deleted_at = excluded.deleted_at,
		logical_id = excluded.logical_id,
		current_revision_id = excluded.current_revision_id`,
		m.ID, m.ProjectID, m.Category, m.Content, m.ContentHash, keywords, embedding,
		m.Source, m.SourceID, m.CreatedAt, m.UpdatedAt, m.AccessCount, m.LastAccessed,
		metadata, m.SyncDirty, m.SyncOrigin, m.Author, m.RemoteProjectKey,
		deletedAt, logicalID, revisionID,
	)
	if err != nil {
		return fmt.Errorf("update current memory view: %w", err)
	}
	return nil
}

func encodeRevisionJSON(m Memory) (keywords any, metadata any, err error) {
	if m.Keywords != nil {
		b, marshalErr := json.Marshal(m.Keywords)
		if marshalErr != nil {
			return nil, nil, fmt.Errorf("marshal keywords: %w", marshalErr)
		}
		keywords = string(b)
	}
	if m.Metadata != nil {
		b, marshalErr := json.Marshal(m.Metadata)
		if marshalErr != nil {
			return nil, nil, fmt.Errorf("marshal metadata: %w", marshalErr)
		}
		metadata = string(b)
	}
	return keywords, metadata, nil
}

type revisionScanner interface {
	Scan(dest ...any) error
}

func scanRevision(scanner revisionScanner) (*MemoryRevision, error) {
	revision, err := scanRevisionValue(scanner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return revision, err
}

func scanRevisionRows(rows *sql.Rows) (*MemoryRevision, error) {
	return scanRevisionValue(rows)
}

func scanRevisionValue(scanner revisionScanner) (*MemoryRevision, error) {
	var r MemoryRevision
	var project, contentHash, keywords, source, sourceID, metadata sql.NullString
	var author, remote sql.NullString
	var validFromNS, systemFromNS int64
	var validToNS, systemToNS sql.NullInt64
	if err := scanner.Scan(
		&r.RevisionID, &r.MemoryID, &r.LogicalID, &project, &r.Memory.Category,
		&r.Memory.Content, &contentHash, &keywords, &source, &sourceID, &metadata,
		&r.Memory.CreatedAt, &r.Memory.UpdatedAt, &author, &remote,
		&r.Mode, &r.IsTombstone, &validFromNS, &validToNS,
		&systemFromNS, &systemToNS,
	); err != nil {
		return nil, err
	}
	r.Memory.ID = r.MemoryID
	r.Memory.ProjectID = nilIfNull(project)
	r.Memory.ContentHash = contentHash.String
	r.Memory.Keywords = unmarshalKeywords(keywords)
	r.Memory.Source = source.String
	r.Memory.SourceID = nilIfNull(sourceID)
	r.Memory.Author = nilIfNull(author)
	r.Memory.RemoteProjectKey = nilIfNull(remote)
	if metadata.Valid {
		_ = json.Unmarshal([]byte(metadata.String), &r.Memory.Metadata)
	}
	r.ValidFrom = time.Unix(0, validFromNS).UTC()
	r.ValidTo = nullNanoTime(validToNS)
	r.SystemFrom = time.Unix(0, systemFromNS).UTC()
	r.SystemTo = nullNanoTime(systemToNS)
	return &r, nil
}

func timeNano(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixNano()
}

func nullNanoTime(t sql.NullInt64) *time.Time {
	if !t.Valid {
		return nil
	}
	result := time.Unix(0, t.Int64).UTC()
	return &result
}

func utcTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	result := t.UTC()
	return &result
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	result := *t
	return &result
}
