package memory

import (
	"context"
	"testing"
)

type captureSaveStore struct {
	Store
	saved Memory
}

func (s *captureSaveStore) Save(_ context.Context, m Memory) error {
	s.saved = m
	return nil
}

func (s *captureSaveStore) FindByNormalizedHash(_ context.Context, _ string, _ *string) (*Memory, error) {
	return nil, nil
}

func (s *captureSaveStore) BackfillNormalizedHash(_ context.Context, _ int) (int, error) { return 0, nil }

func (s *captureSaveStore) PendingNormalizedHash(_ context.Context) (int, error) { return 0, nil }

func (s *captureSaveStore) FindByContentHash(_ context.Context, _ string, _ *string) (*Memory, error) {
	return nil, nil
}

func (s *captureSaveStore) Search(_ context.Context, _ string, _ SearchOptions) ([]SearchResult, error) {
	return nil, nil
}

// Regression: the create path must assign the ID before store.Save, because
// Save takes Memory by value. Otherwise embedAsync/observers run with an empty
// ID and the embedding is never persisted to the memory row.
func TestServiceSaveWithOptions_PropagatesNonEmptyID(t *testing.T) {
	store := &captureSaveStore{}
	svc := &Service{store: store}

	m, err := svc.SaveWithOptions(context.Background(), SaveOptions{
		Content:   "A meaningful, sufficiently long fact worth persisting and embedding.",
		Category:  "fact",
		Source:    "test",
		SkipEmbed: true,
	})
	if err != nil {
		t.Fatalf("SaveWithOptions error: %v", err)
	}
	if m.ID == "" {
		t.Fatal("returned memory has empty ID")
	}
	if store.saved.ID != m.ID {
		t.Fatalf("ID not propagated to store: saved %q, returned %q", store.saved.ID, m.ID)
	}
}
