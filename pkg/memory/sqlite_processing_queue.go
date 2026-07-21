package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var _ ProcessingQueueStore = (*SQLiteStore)(nil)
var _ ProcessingRevisionStore = (*SQLiteStore)(nil)

func insertProcessingJobsTx(
	ctx context.Context,
	tx *sql.Tx,
	revision MemoryRevision,
	specs []ProcessingJobSpec,
	now time.Time,
) error {
	for _, spec := range specs {
		if spec.Kind == "" {
			return fmt.Errorf("processing job kind cannot be empty")
		}
		if spec.MaxAttempts <= 0 {
			spec.MaxAttempts = 5
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_processing_jobs (
			id, revision_id, memory_id, kind, generation, state, attempts,
			max_attempts, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?)`,
			newUUID(), revision.RevisionID, revision.MemoryID, spec.Kind,
			spec.Generation, spec.MaxAttempts, now.UnixNano(), now.UnixNano())
		if err != nil {
			return fmt.Errorf("insert processing job %s: %w", spec.Kind, err)
		}
	}
	return nil
}

func (s *SQLiteStore) ClaimProcessingJob(
	ctx context.Context,
	kind, owner string,
	now time.Time,
	lease time.Duration,
) (*ProcessingJob, error) {
	if owner == "" || lease <= 0 {
		return nil, fmt.Errorf("processing claim requires owner and positive lease")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Expired final attempts become terminal before another worker can claim.
	if _, err := tx.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = 'failed', owner = NULL, lease_until = NULL,
			last_error = CASE WHEN last_error IS NULL OR last_error = ''
				THEN 'lease expired after maximum attempts' ELSE last_error END,
			updated_at = ?
		WHERE state = 'processing' AND lease_until <= ? AND attempts >= max_attempts`,
		now.UnixNano(), now.UnixNano()); err != nil {
		return nil, fmt.Errorf("expire processing jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = 'pending', owner = NULL, lease_until = NULL, updated_at = ?
		WHERE state = 'processing' AND lease_until <= ? AND attempts < max_attempts`,
		now.UnixNano(), now.UnixNano()); err != nil {
		return nil, fmt.Errorf("requeue expired processing jobs: %w", err)
	}

	query := `SELECT id FROM memory_processing_jobs
		WHERE attempts < max_attempts
		  AND state = 'pending'
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`
	args := []any{now.UnixNano()}
	if kind != "" {
		query += " AND kind = ?"
		args = append(args, kind)
	}
	query += " ORDER BY created_at ASC, id ASC LIMIT 1"
	var id string
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("select processing job: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = 'processing', attempts = attempts + 1, owner = ?,
			lease_until = ?, next_attempt_at = NULL, updated_at = ?
		WHERE id = ? AND attempts < max_attempts
		  AND state = 'pending'
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`,
		owner, now.Add(lease).UnixNano(), now.UnixNano(), id, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("claim processing job: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return nil, nil
	}
	job, err := getProcessingJobTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *SQLiteStore) CompleteProcessingJob(ctx context.Context, id, owner string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = 'done', owner = NULL, lease_until = NULL, next_attempt_at = NULL,
			last_error = NULL, completed_at = ?, updated_at = ?
		WHERE id = ? AND state = 'processing' AND owner = ?`,
		now.UTC().UnixNano(), now.UTC().UnixNano(), id, owner)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return nil
	}
	return s.processingOwnerError(ctx, id, JobDone)
}

func (s *SQLiteStore) FailProcessingJob(
	ctx context.Context,
	id, owner, message string,
	now, retryAt time.Time,
) (*ProcessingJob, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := getProcessingJobTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if job.State != JobProcessing || job.Owner != owner {
		return nil, ErrClaimOwnerMismatch
	}
	state := JobPending
	var next any = retryAt.UTC().UnixNano()
	if job.Attempts >= job.MaxAttempts {
		state = JobFailed
		next = nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = ?, owner = NULL, lease_until = NULL, next_attempt_at = ?,
			last_error = ?, updated_at = ?
		WHERE id = ? AND state = 'processing' AND owner = ?`,
		state, next, message, now.UnixNano(), id, owner); err != nil {
		return nil, err
	}
	job, err = getProcessingJobTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *SQLiteStore) RequeueProcessingJob(ctx context.Context, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE memory_processing_jobs
		SET state = 'pending', attempts = 0, owner = NULL, lease_until = NULL,
			next_attempt_at = NULL, last_error = NULL, completed_at = NULL, updated_at = ?
		WHERE id = ? AND state = 'failed'`, now.UTC().UnixNano(), id)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return nil
	}
	return s.queueItemStateError(ctx, "memory_processing_jobs", "id", id, "failed")
}

func (s *SQLiteStore) ProcessingRevision(
	ctx context.Context,
	revisionID string,
) (*MemoryRevision, bool, error) {
	revision, err := scanRevision(s.db.QueryRowContext(
		ctx, revisionSelectSQL+` WHERE r.revision_id = ?`, revisionID,
	))
	if err != nil {
		return nil, false, fmt.Errorf("load processing revision %s: %w", revisionID, err)
	}
	if revision == nil {
		return nil, false, ErrQueueItemNotFound
	}
	var current bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM memories
		WHERE id = ? AND current_revision_id = ? AND deleted_at IS NULL
	)`, revision.MemoryID, revisionID).Scan(&current); err != nil {
		return nil, false, fmt.Errorf("check current processing revision %s: %w", revisionID, err)
	}
	return revision, current, nil
}

func (s *SQLiteStore) UpdateEmbeddingForRevision(
	ctx context.Context,
	memoryID, revisionID string,
	embedding []float32,
) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET embedding = ?
		WHERE id = ? AND current_revision_id = ? AND deleted_at IS NULL`,
		float32sToBlob(embedding), memoryID, revisionID)
	if err != nil {
		return false, fmt.Errorf("update embedding for revision %s: %w", revisionID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if changed == 1 {
		s.cache.Put(memoryID, embedding)
		return true, nil
	}
	return false, nil
}

func (s *SQLiteStore) processingOwnerError(ctx context.Context, id string, idempotent ProcessingJobState) error {
	var state ProcessingJobState
	if err := s.db.QueryRowContext(ctx,
		`SELECT state FROM memory_processing_jobs WHERE id = ?`, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQueueItemNotFound
		}
		return err
	}
	if state == idempotent {
		return nil
	}
	return ErrClaimOwnerMismatch
}

func (s *SQLiteStore) queueItemStateError(
	ctx context.Context,
	table, keyColumn, id, expected string,
) error {
	query := fmt.Sprintf("SELECT state FROM %s WHERE %s = ?", table, keyColumn)
	var state string
	if err := s.db.QueryRowContext(ctx, query, id).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQueueItemNotFound
		}
		return err
	}
	return fmt.Errorf("queue item state is %s, expected %s", state, expected)
}

func getProcessingJobTx(ctx context.Context, tx *sql.Tx, id string) (*ProcessingJob, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, revision_id, memory_id, kind, generation,
		state, attempts, max_attempts, owner, lease_until, next_attempt_at,
		last_error, created_at, updated_at, completed_at
		FROM memory_processing_jobs WHERE id = ?`, id)
	var job ProcessingJob
	var owner, lastError sql.NullString
	var leaseUntil, nextAttempt, completed sql.NullInt64
	var created, updated int64
	if err := row.Scan(&job.ID, &job.RevisionID, &job.MemoryID, &job.Kind, &job.Generation,
		&job.State, &job.Attempts, &job.MaxAttempts, &owner, &leaseUntil, &nextAttempt,
		&lastError, &created, &updated, &completed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrQueueItemNotFound
		}
		return nil, err
	}
	job.Owner = owner.String
	job.LastError = lastError.String
	job.LeaseUntil = nullNanoTime(leaseUntil)
	job.NextAttemptAt = nullNanoTime(nextAttempt)
	job.CreatedAt = time.Unix(0, created).UTC()
	job.UpdatedAt = time.Unix(0, updated).UTC()
	job.CompletedAt = nullNanoTime(completed)
	return &job, nil
}
