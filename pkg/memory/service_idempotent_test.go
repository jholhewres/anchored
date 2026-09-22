package memory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func countRevisions(t *testing.T, store *SQLiteStore, memoryID string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memory_revisions WHERE memory_id = ?`, memoryID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newIdempotencyTestService(t *testing.T) (*Service, *SQLiteStore) {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "idempotent.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		store:    store,
		projDet:  nil,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		embedSem: make(chan struct{}, 1),
		shutdown: make(chan struct{}),
	}
	t.Cleanup(func() { store.Close() })
	return svc, store
}

// Re-saving the same memory is the most common write an agent makes. Before the
// guard each one appended a revision plus a verbatim copy of the embedding,
// which is how 84k memories became 1M revisions.
func TestSaveIsIdempotentWhenNothingChanged(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)

	first, err := svc.Save(ctx, "anchored runs as a single static binary", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	after := countRevisions(t, store, first.ID)

	for i := 0; i < 5; i++ {
		again, err := svc.Save(ctx, "anchored runs as a single static binary", "fact", "test", "")
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != first.ID {
			t.Fatalf("re-save created memory %s, want %s", again.ID, first.ID)
		}
	}

	if got := countRevisions(t, store, first.ID); got != after {
		t.Fatalf("revisions after 5 identical re-saves = %d, want %d", got, after)
	}
}

// The guard must not swallow a distinct save. Different content is a different
// memory (near-duplicate matching only folds case/whitespace), so the check is
// that a second memory exists — not that the first one grew a revision.
func TestSaveStillRecordsDistinctContent(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)

	first, err := svc.Save(ctx, "anchored ships as one binary", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Save(ctx, "anchored ships as one binary with zero dependencies", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("distinct content should not fold into the existing memory")
	}

	var n int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("memories = %d, want 2", n)
	}
}

// An in-place edit goes through UpdateTemporal, which must still supersede.
func TestUpdateStillRecordsRevision(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)

	first, err := svc.Save(ctx, "anchored ships as one binary", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	before := countRevisions(t, store, first.ID)

	if _, err := svc.Update(ctx, first.ID, "anchored ships as one static binary", ""); err != nil {
		t.Fatal(err)
	}

	if got := countRevisions(t, store, first.ID); got <= before {
		t.Fatalf("revisions after an update = %d, want more than %d", got, before)
	}
}

// A category change is a content-stable edit the ledger still has to record.
func TestSaveRecordsCategoryChange(t *testing.T) {
	ctx := context.Background()
	svc, store := newIdempotencyTestService(t)

	first, err := svc.Save(ctx, "we settled on sqlite for the store", "fact", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	before := countRevisions(t, store, first.ID)

	if _, err := svc.Save(ctx, "we settled on sqlite for the store", "decision", "test", ""); err != nil {
		t.Fatal(err)
	}

	if got := countRevisions(t, store, first.ID); got <= before {
		t.Fatalf("revisions after a category change = %d, want more than %d", got, before)
	}
}

func TestSameStoredMemoryComparesMetadataAcrossRepresentations(t *testing.T) {
	typed := Memory{
		ID: "m1", Content: "c", Category: "fact", ContentHash: "h",
		Metadata: MemoryMetadata{Kind: "fact", Origin: OriginManual}.ToAny(),
	}
	// What a store read hands back: the same JSON decoded into a generic map.
	decoded := typed
	normalized, ok := normalizeMetadata(typed.Metadata)
	if !ok {
		t.Fatal("normalize failed")
	}
	decoded.Metadata = normalized

	if !sameStoredMemory(typed, decoded) {
		t.Fatal("typed and decoded metadata should compare equal")
	}

	changed := decoded
	changed.Metadata = MemoryMetadata{Kind: "decision", Origin: OriginManual}.ToAny()
	if sameStoredMemory(typed, changed) {
		t.Fatal("different metadata should not compare equal")
	}
}

func TestSameStoredMemoryDetectsFieldChanges(t *testing.T) {
	base := Memory{ID: "m1", Content: "c", Category: "fact", Source: "s", ContentHash: "h"}

	for name, mutate := range map[string]func(Memory) Memory{
		"content":  func(m Memory) Memory { m.Content = "other"; return m },
		"category": func(m Memory) Memory { m.Category = "decision"; return m },
		"source":   func(m Memory) Memory { m.Source = "other"; return m },
		"sourceID": func(m Memory) Memory { id := "x"; m.SourceID = &id; return m },
		"hash":     func(m Memory) Memory { m.ContentHash = "other"; return m },
		"keywords": func(m Memory) Memory { m.Keywords = []string{"k"}; return m },
		"project":  func(m Memory) Memory { p := "p"; m.ProjectID = &p; return m },
	} {
		if sameStoredMemory(base, mutate(base)) {
			t.Fatalf("%s change should not compare equal", name)
		}
	}

	if !sameStoredMemory(base, base) {
		t.Fatal("identical memories should compare equal")
	}
}
