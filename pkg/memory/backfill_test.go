package memory

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"testing"
)

// backfillTestStore embeds the hybrid mock store and simulates a queue of
// memories missing a vector: ListWithoutEmbedding returns the not-yet-updated
// ones (bounded by limit) and UpdateEmbedding marks them done, so the drain
// loop terminates exactly like the real store.
type backfillTestStore struct {
	*hybridMockStore
	pending []Memory
	updated map[string]bool
}

func (s *backfillTestStore) ListWithoutEmbedding(ctx context.Context, limit int) ([]Memory, error) {
	var out []Memory
	for _, m := range s.pending {
		if s.updated[m.ID] {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *backfillTestStore) UpdateEmbedding(ctx context.Context, id string, e []float32) error {
	s.updated[id] = true
	return nil
}

func (s *backfillTestStore) CountWithoutEmbedding(ctx context.Context) (int, error) {
	n := 0
	for _, m := range s.pending {
		if !s.updated[m.ID] {
			n++
		}
	}
	return n, nil
}

func newBackfillStore(n int) *backfillTestStore {
	mems := make([]Memory, n)
	for i := 0; i < n; i++ {
		mems[i] = Memory{ID: fmt.Sprintf("m%d", i), Content: fmt.Sprintf("content %d", i)}
	}
	return &backfillTestStore{hybridMockStore: &hybridMockStore{}, pending: mems, updated: map[string]bool{}}
}

// embeddingCacheDB opens an in-memory sqlite with just the embedding_cache
// table so NewEmbeddingCache.Put works without the full migration stack.
func embeddingCacheDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE embedding_cache (
		text_hash TEXT, model TEXT, embedding BLOB, PRIMARY KEY (text_hash, model))`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newBackfillService(store Store, db *sql.DB) *Service {
	return &Service{
		store:    store,
		embedder: &vec4Embedder{},
		cache:    NewEmbeddingCache(db, slog.New(slog.NewTextHandler(io.Discard, nil))),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestBackfillEmbeddingsThrottled_DrainsAll(t *testing.T) {
	store := newBackfillStore(5)
	svc := newBackfillService(store, embeddingCacheDB(t))

	n, err := svc.BackfillEmbeddingsThrottled(context.Background(), 2, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 5 {
		t.Fatalf("want 5 embedded, got %d", n)
	}
	if len(store.updated) != 5 {
		t.Fatalf("want 5 rows persisted, got %d", len(store.updated))
	}
}

func TestBackfillEmbeddingsThrottled_StopsOnCancelledContext(t *testing.T) {
	store := newBackfillStore(100)
	svc := newBackfillService(store, embeddingCacheDB(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: must return before embedding anything

	n, err := svc.BackfillEmbeddingsThrottled(ctx, 10, 0)
	if err == nil {
		t.Fatal("want a context error, got nil")
	}
	if n != 0 {
		t.Fatalf("want 0 embedded on pre-cancel, got %d", n)
	}
}

func TestBackfillEmbeddings_NoEmbedderIsError(t *testing.T) {
	svc := &Service{store: newBackfillStore(1), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := svc.BackfillEmbeddings(context.Background(), 10); err == nil {
		t.Fatal("want error when embedder unavailable, got nil")
	}
}
