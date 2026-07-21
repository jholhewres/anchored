package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type OutboxState string

const (
	OutboxPending    OutboxState = "pending"
	OutboxProcessing OutboxState = "processing"
	OutboxDelivered  OutboxState = "delivered"
	OutboxDeadLetter OutboxState = "dead_letter"
)

type RemoteOutboxSpec struct {
	OperationID   string
	Remote        string
	Project       string
	PayloadHash   string
	Payload       []byte
	MaxAttempts   int
	NextAttemptAt *time.Time
}

// DurableSaveOptions is additive so SaveOptions keeps its original exported
// shape for downstream callers that use positional composite literals.
type DurableSaveOptions struct {
	SaveOptions
	RemoteOutbox       []RemoteOutboxSpec
	DeriveRemoteOutbox func(Memory) ([]RemoteOutboxSpec, error)
}

type RemoteOutboxItem struct {
	OperationID   string
	MemoryID      string
	RevisionID    string
	Remote        string
	Project       string
	PayloadHash   string
	Payload       []byte
	State         OutboxState
	Attempts      int
	MaxAttempts   int
	Owner         string
	LeaseUntil    *time.Time
	NextAttemptAt *time.Time
	ErrorClass    string
	LastError     string
	Ack           []byte
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeliveredAt   *time.Time
}

type OutboxDisposition string

const (
	OutboxDispositionDelivered OutboxDisposition = "delivered"
	OutboxDispositionRetry     OutboxDisposition = "retry"
	OutboxDispositionPermanent OutboxDisposition = "permanent"
)

type RemoteOutboxStore interface {
	ClaimRemoteOutbox(ctx context.Context, owner string, now time.Time, lease time.Duration) (*RemoteOutboxItem, error)
	DeliverRemoteOutbox(ctx context.Context, operationID, owner string, ack []byte, now time.Time) error
	FailRemoteOutbox(ctx context.Context, operationID, owner, errorClass, message string, now, retryAt time.Time, permanent bool) (*RemoteOutboxItem, error)
	RequeueRemoteOutbox(ctx context.Context, operationID string, now time.Time) error
}

// RemoteOutboxDeliveryResult is the transport-independent outcome returned by
// the delivery adapter registered by the MCP layer.
type RemoteOutboxDeliveryResult struct {
	StatusCode          int
	Ack                 []byte
	RetryAfter          time.Duration
	SamePayloadConflict bool
	TransportError      error
}

type RemoteOutboxDeliverer func(context.Context, RemoteOutboxItem) RemoteOutboxDeliveryResult

// ClassifyOutboxResult implements the protocol error matrix. transportFailure
// is separate from status so callers never have to invent an HTTP status.
func ClassifyOutboxResult(status int, samePayloadConflict, transportFailure bool) OutboxDisposition {
	if transportFailure {
		return OutboxDispositionRetry
	}
	if status >= 200 && status < 300 {
		return OutboxDispositionDelivered
	}
	if status == 409 {
		if samePayloadConflict {
			return OutboxDispositionDelivered
		}
		return OutboxDispositionPermanent
	}
	if status == 408 || status == 425 || status == 429 || status >= 500 {
		return OutboxDispositionRetry
	}
	return OutboxDispositionPermanent
}

const maxOutboxBackoff = time.Hour

// OutboxRetryAt honors Retry-After and otherwise uses capped exponential
// backoff. attempt is one-based (the value persisted by claim).
func OutboxRetryAt(now time.Time, attempt int, retryAfter time.Duration) time.Time {
	return OutboxRetryAtFor("", now, attempt, retryAfter)
}

// OutboxRetryAtFor adds deterministic ±20% jitter keyed by immutable
// operation ID. Determinism avoids synchronized retry storms while keeping
// tests and restart behavior reproducible. Retry-After is honored without
// negative jitter, so a server-requested minimum delay is never violated.
func OutboxRetryAtFor(operationID string, now time.Time, attempt int, retryAfter time.Duration) time.Time {
	delay := retryAfter
	if delay <= 0 {
		if attempt < 1 {
			attempt = 1
		}
		delay = time.Second
		for i := 1; i < attempt && delay < maxOutboxBackoff; i++ {
			delay *= 2
		}
		if delay > maxOutboxBackoff {
			delay = maxOutboxBackoff
		}
		seed := sha256.Sum256([]byte(operationID + "\x00" + time.Duration(attempt).String()))
		bucket := int(seed[0])<<8 | int(seed[1])
		basisPoints := 8000 + bucket*4000/65535
		delay = delay * time.Duration(basisPoints) / 10000
	}
	if delay > maxOutboxBackoff {
		delay = maxOutboxBackoff
	}
	return now.UTC().Add(delay)
}

func normalizeOutboxSpec(spec RemoteOutboxSpec, revisionID string) RemoteOutboxSpec {
	if spec.PayloadHash == "" {
		sum := sha256.Sum256(spec.Payload)
		spec.PayloadHash = hex.EncodeToString(sum[:])
	}
	if spec.OperationID == "" {
		sum := sha256.Sum256([]byte(revisionID + "\x00" + spec.Remote + "\x00" + spec.Project + "\x00" + spec.PayloadHash))
		spec.OperationID = hex.EncodeToString(sum[:])
	}
	if spec.MaxAttempts <= 0 {
		spec.MaxAttempts = 8
	}
	return spec
}
