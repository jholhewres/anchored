package memory

import (
	"context"
	"testing"
)

type capturePreferenceScopeStore struct {
	Store
	saved    Memory
	existing *Memory
}

func (s *capturePreferenceScopeStore) Save(_ context.Context, m Memory) error {
	s.saved = m
	return nil
}

func (s *capturePreferenceScopeStore) FindByNormalizedHash(_ context.Context, _ string, _ *string) (*Memory, error) {
	return nil, nil
}

func (s *capturePreferenceScopeStore) BackfillNormalizedHash(_ context.Context, _ int) (int, error) { return 0, nil }

func (s *capturePreferenceScopeStore) PendingNormalizedHash(_ context.Context) (int, error) { return 0, nil }

func (s *capturePreferenceScopeStore) FindByContentHash(_ context.Context, _ string, _ *string) (*Memory, error) {
	return s.existing, nil
}

// Search is exercised by the near-duplicate check on the save path; this mock
// has no candidates to return.
func (s *capturePreferenceScopeStore) Search(_ context.Context, _ string, _ SearchOptions) ([]SearchResult, error) {
	return nil, nil
}

func TestServiceSaveWithOptions_DefaultsPreferenceScopeToUser(t *testing.T) {
	store := &capturePreferenceScopeStore{}
	svc := &Service{store: store}

	m, err := svc.SaveWithOptions(context.Background(), SaveOptions{
		Content:  "Prefer small PRs",
		Category: "preference",
		Source:   "test",
	})
	if err != nil {
		t.Fatalf("SaveWithOptions error: %v", err)
	}

	metadata := ParseMetadata(m.Metadata)
	if metadata.PreferenceScope != PreferenceScopeUser {
		t.Fatalf("returned memory preference scope: got %q want %q", metadata.PreferenceScope, PreferenceScopeUser)
	}

	savedMetadata := ParseMetadata(store.saved.Metadata)
	if savedMetadata.PreferenceScope != PreferenceScopeUser {
		t.Fatalf("saved memory preference scope: got %q want %q", savedMetadata.PreferenceScope, PreferenceScopeUser)
	}
}

func TestServiceSaveWithOptions_UsesExplicitPreferenceScope(t *testing.T) {
	store := &capturePreferenceScopeStore{}
	svc := &Service{store: store}

	m, err := svc.SaveWithOptions(context.Background(), SaveOptions{
		Content:         "We prefer short-lived branches on this project",
		Category:        "preference",
		Source:          "test",
		PreferenceScope: PreferenceScopeProject,
	})
	if err != nil {
		t.Fatalf("SaveWithOptions error: %v", err)
	}

	metadata := ParseMetadata(m.Metadata)
	if metadata.PreferenceScope != PreferenceScopeProject {
		t.Fatalf("returned memory preference scope: got %q want %q", metadata.PreferenceScope, PreferenceScopeProject)
	}
}

func TestServiceSaveWithOptions_DoesNotSetScopeForNonPreference(t *testing.T) {
	store := &capturePreferenceScopeStore{}
	svc := &Service{store: store}

	m, err := svc.SaveWithOptions(context.Background(), SaveOptions{
		Content:         "We chose Postgres",
		Category:        "decision",
		Source:          "test",
		PreferenceScope: PreferenceScopeTeam,
	})
	if err != nil {
		t.Fatalf("SaveWithOptions error: %v", err)
	}

	metadata := ParseMetadata(m.Metadata)
	if metadata.PreferenceScope != "" {
		t.Fatalf("expected non-preference scope to remain empty, got %q", metadata.PreferenceScope)
	}
	if metadata.QualityScore == 0 {
		t.Fatalf("expected non-preference memory to receive quality metadata")
	}
}

func TestServiceSaveWithOptions_PreservesExistingMetadataWhenUpdatingDuplicatePreference(t *testing.T) {
	store := &capturePreferenceScopeStore{
		existing: &Memory{
			ID:       "mem_existing",
			Metadata: MemoryMetadata{Source: "import"}.ToAny(),
		},
	}
	svc := &Service{store: store}

	m, err := svc.SaveWithOptions(context.Background(), SaveOptions{
		Content:         "Prefer small PRs",
		Category:        "preference",
		Source:          "test",
		PreferenceScope: PreferenceScopeTeam,
	})
	if err != nil {
		t.Fatalf("SaveWithOptions error: %v", err)
	}

	metadata := ParseMetadata(m.Metadata)
	if metadata.Source != "import" {
		t.Fatalf("expected existing metadata source to be preserved, got %q", metadata.Source)
	}
	if metadata.PreferenceScope != PreferenceScopeTeam {
		t.Fatalf("expected updated preference scope %q, got %q", PreferenceScopeTeam, metadata.PreferenceScope)
	}
}
