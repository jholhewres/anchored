package memory

import (
	"context"
	"errors"
	"time"
)

var ErrDerivedWorkHealthUnavailable = errors.New("derived work health is unavailable")

type QueueStateHealth struct {
	Counts        map[string]int64 `json:"counts"`
	OldestPending *time.Time       `json:"oldest_pending,omitempty"`
}

type DerivedWorkHealth struct {
	Processing QueueStateHealth `json:"processing"`
	Outbox     QueueStateHealth `json:"outbox"`
}

// DerivedWorkHealthStore is optional so alternate Store implementations and
// existing test fakes remain source-compatible.
type DerivedWorkHealthStore interface {
	DerivedWorkHealth(ctx context.Context) (DerivedWorkHealth, error)
}

func (s *Service) DerivedWorkHealth(ctx context.Context) (DerivedWorkHealth, error) {
	store, ok := s.store.(DerivedWorkHealthStore)
	if !ok {
		return DerivedWorkHealth{}, ErrDerivedWorkHealthUnavailable
	}
	return store.DerivedWorkHealth(ctx)
}
