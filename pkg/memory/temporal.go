package memory

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TemporalMode describes why a new immutable memory revision was recorded.
type TemporalMode string

const (
	// TemporalSupersede records a real-world change. Existing valid-time
	// intervals are split around the replacement interval.
	TemporalSupersede TemporalMode = "supersede"
	// TemporalCorrect records a correction to what Anchored knew while
	// preserving the corrected assertion's valid-time interval.
	TemporalCorrect TemporalMode = "correct"
	// TemporalTombstone records a non-destructive terminal revision.
	TemporalTombstone TemporalMode = "tombstone"
)

var (
	ErrTemporalIdentityRequired = errors.New("explicit temporal write requires logical identity")
	ErrInvalidTemporalInterval  = errors.New("temporal interval end must be after start")
	ErrTemporalOverlap          = errors.New("temporal revisions overlap on valid and system time")
	ErrCorrectionIntervalChange = errors.New("correction cannot change the valid-time interval")
)

// TemporalWriteOptions adds temporal semantics without changing Store.Save.
// A zero value is the legacy-compatible behavior: supersede at transaction
// start, using Memory.ID as the logical identity.
type TemporalWriteOptions struct {
	Mode      TemporalMode
	LogicalID string
	ValidFrom *time.Time
	ValidTo   *time.Time
	// ProcessingJobs and RemoteOutbox are optional durable derived-work specs.
	// SQLite commits them in the same transaction as the revision/current view.
	ProcessingJobs []ProcessingJobSpec
	RemoteOutbox   []RemoteOutboxSpec
}

// TemporalQueryOptions selects a revision on valid and system/known time.
//
// Defaults are captured once at query start:
//   - neither axis: resolve the current compatibility-view revision;
//   - valid_at only: known_at is query start;
//   - known_at only: valid_at equals known_at.
type TemporalQueryOptions struct {
	ValidAt          *time.Time
	KnownAt          *time.Time
	IncludeTombstone bool
}

// MemoryRevision is an immutable content snapshot. Only interval end markers
// may be closed when later knowledge arrives; prior payload fields never change.
type MemoryRevision struct {
	RevisionID  string       `json:"revision_id"`
	MemoryID    string       `json:"memory_id"`
	LogicalID   string       `json:"logical_id"`
	Memory      Memory       `json:"memory"`
	Mode        TemporalMode `json:"temporal_mode"`
	IsTombstone bool         `json:"is_tombstone"`
	ValidFrom   time.Time    `json:"valid_from"`
	ValidTo     *time.Time   `json:"valid_to,omitempty"`
	SystemFrom  time.Time    `json:"system_from"`
	SystemTo    *time.Time   `json:"system_to,omitempty"`
}

// TemporalStore is an optional capability. Store remains unchanged so existing
// fakes and alternate stores continue to compile and retain legacy behavior.
type TemporalStore interface {
	SaveTemporal(ctx context.Context, m Memory, opts TemporalWriteOptions) (*MemoryRevision, error)
	RevisionAt(ctx context.Context, logicalID string, opts TemporalQueryOptions) (*MemoryRevision, error)
	ListRevisions(ctx context.Context, logicalID string) ([]MemoryRevision, error)
}

// TemporalMutation applies a partial current-view mutation after acquiring the
// SQLite write transaction, preventing a read/modify/write lost update.
type TemporalMutation struct {
	Content        *string
	Category       *string
	Metadata       any
	SetMetadata    bool
	ClearEmbedding bool
	ExpectedState  TemporalMutationState
}

// TemporalMutationState makes active/tombstone transitions explicit. A state
// mismatch is a compatibility no-op, matching the old UPDATE ... deleted_at
// predicates without opening a read-then-write race.
type TemporalMutationState string

const (
	TemporalStateAny       TemporalMutationState = ""
	TemporalStateActive    TemporalMutationState = "active"
	TemporalStateTombstone TemporalMutationState = "tombstone"
)

// TemporalMutationStore is a narrower optional capability for atomic partial
// updates. It stays separate from Store and TemporalStore for source
// compatibility with existing fakes and alternate stores.
type TemporalMutationStore interface {
	UpdateTemporal(ctx context.Context, id string, mutation TemporalMutation, opts TemporalWriteOptions) (*MemoryRevision, error)
}

func normalizeTemporalMode(mode TemporalMode) (TemporalMode, error) {
	if mode == "" {
		return TemporalSupersede, nil
	}
	switch mode {
	case TemporalSupersede, TemporalCorrect, TemporalTombstone:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown temporal mode %q", mode)
	}
}

func validTemporalInterval(start time.Time, end *time.Time) error {
	if end != nil && !start.Before(*end) {
		return ErrInvalidTemporalInterval
	}
	return nil
}

func intervalsOverlap(aStart time.Time, aEnd *time.Time, bStart time.Time, bEnd *time.Time) bool {
	return (aEnd == nil || bStart.Before(*aEnd)) &&
		(bEnd == nil || aStart.Before(*bEnd))
}
