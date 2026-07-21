package memory

import (
	"context"
	"errors"
	"time"
)

type ProcessingJobState string

const (
	JobPending    ProcessingJobState = "pending"
	JobProcessing ProcessingJobState = "processing"
	JobDone       ProcessingJobState = "done"
	JobFailed     ProcessingJobState = "failed"
)

var (
	ErrClaimOwnerMismatch = errors.New("claim owner does not match active lease")
	ErrQueueItemNotFound  = errors.New("queue item not found")
)

type ProcessingJobSpec struct {
	Kind        string
	Generation  string
	MaxAttempts int
}

type ProcessingJob struct {
	ID            string
	RevisionID    string
	MemoryID      string
	Kind          string
	Generation    string
	State         ProcessingJobState
	Attempts      int
	MaxAttempts   int
	Owner         string
	LeaseUntil    *time.Time
	NextAttemptAt *time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CompletedAt   *time.Time
}

// ProcessingQueueStore is optional; Store and its existing fakes remain
// source-compatible.
type ProcessingQueueStore interface {
	ClaimProcessingJob(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (*ProcessingJob, error)
	CompleteProcessingJob(ctx context.Context, id, owner string, now time.Time) error
	FailProcessingJob(ctx context.Context, id, owner, message string, now, retryAt time.Time) (*ProcessingJob, error)
	RequeueProcessingJob(ctx context.Context, id string, now time.Time) error
}

// ProcessingRevisionStore supplies the immutable input for a claimed job and
// guards application to the current compatibility view. Keeping this separate
// preserves compatibility with non-SQLite queue implementations.
type ProcessingRevisionStore interface {
	ProcessingRevision(ctx context.Context, revisionID string) (revision *MemoryRevision, current bool, err error)
	UpdateEmbeddingForRevision(ctx context.Context, memoryID, revisionID string, embedding []float32) (updated bool, err error)
}
