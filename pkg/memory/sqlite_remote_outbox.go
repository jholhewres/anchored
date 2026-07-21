package memory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var _ RemoteOutboxStore = (*SQLiteStore)(nil)

func cancelRemoteOutboxForMemoryTx(
	ctx context.Context,
	tx *sql.Tx,
	memoryID string,
	now time.Time,
) error {
	_, err := tx.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'dead_letter', owner = NULL, lease_until = NULL,
			next_attempt_at = NULL, error_class = 'local_tombstone',
			last_error = 'cancelled because the local memory was soft-deleted',
			updated_at = ?
		WHERE memory_id = ? AND state IN ('pending', 'processing')`,
		now.UTC().UnixNano(), memoryID,
	)
	if err != nil {
		return fmt.Errorf("cancel remote outbox for tombstone: %w", err)
	}
	return nil
}

func insertRemoteOutboxTx(
	ctx context.Context,
	tx *sql.Tx,
	revision MemoryRevision,
	specs []RemoteOutboxSpec,
	now time.Time,
) error {
	for _, raw := range specs {
		spec := normalizeOutboxSpec(raw, revision.RevisionID)
		if spec.Remote == "" {
			return fmt.Errorf("remote outbox destination cannot be empty")
		}
		var next any
		if spec.NextAttemptAt != nil {
			next = spec.NextAttemptAt.UTC().UnixNano()
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO remote_outbox (
			operation_id, memory_id, revision_id, remote, project, payload_hash,
			payload_snapshot, state, attempts, max_attempts, next_attempt_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?, ?)`,
			spec.OperationID, revision.MemoryID, revision.RevisionID, spec.Remote,
			spec.Project, spec.PayloadHash, append([]byte(nil), spec.Payload...),
			spec.MaxAttempts, next, now.UnixNano(), now.UnixNano())
		if err != nil {
			return fmt.Errorf("insert remote outbox %s: %w", spec.OperationID, err)
		}
		if changed, _ := result.RowsAffected(); changed == 0 {
			var revisionID, remote, project, payloadHash string
			var payload []byte
			if err := tx.QueryRowContext(ctx, `SELECT revision_id, remote, project, payload_hash, payload_snapshot
				FROM remote_outbox WHERE operation_id = ?`, spec.OperationID).
				Scan(&revisionID, &remote, &project, &payloadHash, &payload); err != nil {
				return err
			}
			if revisionID != revision.RevisionID || remote != spec.Remote ||
				project != spec.Project || payloadHash != spec.PayloadHash ||
				!bytes.Equal(payload, spec.Payload) {
				return fmt.Errorf("outbox operation %s conflicts with immutable envelope", spec.OperationID)
			}
		}
	}
	return nil
}

func (s *SQLiteStore) ClaimRemoteOutbox(
	ctx context.Context,
	owner string,
	now time.Time,
	lease time.Duration,
) (*RemoteOutboxItem, error) {
	if owner == "" || lease <= 0 {
		return nil, fmt.Errorf("outbox claim requires owner and positive lease")
	}
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'dead_letter', owner = NULL, lease_until = NULL,
			error_class = COALESCE(NULLIF(error_class, ''), 'attempt_limit'),
			last_error = COALESCE(NULLIF(last_error, ''), 'lease expired after maximum attempts'),
			updated_at = ?
		WHERE state = 'processing' AND lease_until <= ? AND attempts >= max_attempts`,
		now.UnixNano(), now.UnixNano()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'pending', owner = NULL, lease_until = NULL, updated_at = ?
		WHERE state = 'processing' AND lease_until <= ? AND attempts < max_attempts`,
		now.UnixNano(), now.UnixNano()); err != nil {
		return nil, err
	}
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT candidate.operation_id
		FROM remote_outbox candidate
		WHERE candidate.attempts < candidate.max_attempts
		  AND candidate.state = 'pending'
		  AND (candidate.next_attempt_at IS NULL OR candidate.next_attempt_at <= ?)
		  AND NOT EXISTS (
			SELECT 1 FROM remote_outbox prior
			WHERE prior.memory_id = candidate.memory_id
			  AND prior.remote = candidate.remote
			  AND prior.project = candidate.project
			  AND prior.state NOT IN ('delivered', 'dead_letter')
			  AND (
				prior.created_at < candidate.created_at
				OR (prior.created_at = candidate.created_at
					AND prior.operation_id < candidate.operation_id)
			  )
		  )
		ORDER BY candidate.created_at ASC, candidate.operation_id ASC LIMIT 1`,
		now.UnixNano()).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'processing', attempts = attempts + 1, owner = ?,
			lease_until = ?, next_attempt_at = NULL, updated_at = ?
		WHERE operation_id = ? AND attempts < max_attempts
		  AND state = 'pending'
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`,
		owner, now.Add(lease).UnixNano(), now.UnixNano(), id, now.UnixNano())
	if err != nil {
		return nil, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return nil, nil
	}
	item, err := getRemoteOutboxTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *SQLiteStore) DeliverRemoteOutbox(
	ctx context.Context,
	operationID, owner string,
	ack []byte,
	now time.Time,
) error {
	result, err := s.db.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'delivered', owner = NULL, lease_until = NULL,
			next_attempt_at = NULL, error_class = NULL, last_error = NULL,
			ack = ?, delivered_at = ?, updated_at = ?
		WHERE operation_id = ? AND state = 'processing' AND owner = ?`,
		append([]byte(nil), ack...), now.UTC().UnixNano(), now.UTC().UnixNano(),
		operationID, owner)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return nil
	}
	var state OutboxState
	if err := s.db.QueryRowContext(ctx,
		`SELECT state FROM remote_outbox WHERE operation_id = ?`, operationID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQueueItemNotFound
		}
		return err
	}
	if state == OutboxDelivered {
		return nil
	}
	return ErrClaimOwnerMismatch
}

func (s *SQLiteStore) FailRemoteOutbox(
	ctx context.Context,
	operationID, owner, errorClass, message string,
	now, retryAt time.Time,
	permanent bool,
) (*RemoteOutboxItem, error) {
	now = now.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	item, err := getRemoteOutboxTx(ctx, tx, operationID)
	if err != nil {
		return nil, err
	}
	if item.State != OutboxProcessing || item.Owner != owner {
		return nil, ErrClaimOwnerMismatch
	}
	state := OutboxPending
	var next any = retryAt.UTC().UnixNano()
	if permanent || item.Attempts >= item.MaxAttempts {
		state = OutboxDeadLetter
		next = nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_outbox
		SET state = ?, owner = NULL, lease_until = NULL, next_attempt_at = ?,
			error_class = ?, last_error = ?, updated_at = ?
		WHERE operation_id = ? AND state = 'processing' AND owner = ?`,
		state, next, errorClass, message, now.UnixNano(), operationID, owner); err != nil {
		return nil, err
	}
	item, err = getRemoteOutboxTx(ctx, tx, operationID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *SQLiteStore) RequeueRemoteOutbox(ctx context.Context, operationID string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE remote_outbox
		SET state = 'pending', attempts = 0, owner = NULL, lease_until = NULL,
			next_attempt_at = NULL, error_class = NULL, last_error = NULL,
			ack = NULL, delivered_at = NULL, updated_at = ?
		WHERE operation_id = ? AND state = 'dead_letter'
		  AND COALESCE(error_class, '') <> 'local_tombstone'`,
		now.UTC().UnixNano(), operationID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 1 {
		return nil
	}
	return s.queueItemStateError(ctx, "remote_outbox", "operation_id", operationID, "dead_letter")
}

func getRemoteOutboxTx(ctx context.Context, tx *sql.Tx, operationID string) (*RemoteOutboxItem, error) {
	row := tx.QueryRowContext(ctx, `SELECT operation_id, memory_id, revision_id,
		remote, project, payload_hash, payload_snapshot, state, attempts,
		max_attempts, owner, lease_until, next_attempt_at, error_class,
		last_error, ack, created_at, updated_at, delivered_at
		FROM remote_outbox WHERE operation_id = ?`, operationID)
	var item RemoteOutboxItem
	var owner, errorClass, lastError sql.NullString
	var leaseUntil, nextAttempt, delivered sql.NullInt64
	var ack []byte
	var created, updated int64
	if err := row.Scan(&item.OperationID, &item.MemoryID, &item.RevisionID,
		&item.Remote, &item.Project, &item.PayloadHash, &item.Payload, &item.State,
		&item.Attempts, &item.MaxAttempts, &owner, &leaseUntil, &nextAttempt,
		&errorClass, &lastError, &ack, &created, &updated, &delivered); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrQueueItemNotFound
		}
		return nil, err
	}
	item.Owner = owner.String
	item.ErrorClass = errorClass.String
	item.LastError = lastError.String
	item.Ack = append([]byte(nil), ack...)
	item.LeaseUntil = nullNanoTime(leaseUntil)
	item.NextAttemptAt = nullNanoTime(nextAttempt)
	item.CreatedAt = time.Unix(0, created).UTC()
	item.UpdatedAt = time.Unix(0, updated).UTC()
	item.DeliveredAt = nullNanoTime(delivered)
	return &item, nil
}
