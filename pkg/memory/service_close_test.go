package memory

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

type countingCloseStore struct {
	*hybridMockStore
	closes atomic.Int32
}

func (s *countingCloseStore) Close() error {
	s.closes.Add(1)
	return nil
}

type countingCloseEmbedder struct {
	closes atomic.Int32
}

func (*countingCloseEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}

func (*countingCloseEmbedder) Dimensions() int { return 1 }
func (*countingCloseEmbedder) Name() string    { return "close-test" }
func (*countingCloseEmbedder) Model() string   { return "close-test" }

func (e *countingCloseEmbedder) Close() error {
	e.closes.Add(1)
	return nil
}

func TestServiceCloseIsFullyIdempotent(t *testing.T) {
	store := &countingCloseStore{
		hybridMockStore: &hybridMockStore{memories: map[string]Memory{}},
	}
	embedder := &countingCloseEmbedder{}
	service := &Service{
		store:    store,
		embedder: embedder,
		shutdown: make(chan struct{}),
	}

	var callers sync.WaitGroup
	for range 8 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			service.Close()
		}()
	}
	callers.Wait()

	if got := store.closes.Load(); got != 1 {
		t.Fatalf("store Close calls = %d, want 1", got)
	}
	if got := embedder.closes.Load(); got != 1 {
		t.Fatalf("embedder Close calls = %d, want 1", got)
	}
}
