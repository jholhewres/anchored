package memory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceSavePersistsAndDrainsDurableJobs(t *testing.T) {
	store := newTemporalTestStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &Service{
		store:    store,
		embedder: &vec4Embedder{},
		cache:    NewEmbeddingCache(store.DB(), logger),
		logger:   logger,
		shutdown: make(chan struct{}),
	}
	defer svc.Close()

	m, err := svc.SaveDurable(context.Background(), DurableSaveOptions{
		SaveOptions: SaveOptions{
			Content:  "Anchored uses SQLite for durable memory processing",
			Category: "fact",
			Source:   "test",
		},
		RemoteOutbox: []RemoteOutboxSpec{{
			Remote: "remote-a", Project: "project-a", Payload: []byte("snapshot"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForQueueState(t, store, embeddingJobKind, JobDone, 3*time.Second)
	current, err := store.Get(context.Background(), m.ID)
	if err != nil || current == nil || len(current.Embedding) == 0 {
		t.Fatalf("durable embedding was not applied: memory=%#v err=%v", current, err)
	}
	assertTableCount(t, store, "remote_outbox", 1)
}

func TestServiceStartupReclaimsExpiredEmbeddingJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := NewSQLiteStore(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-2 * time.Minute)
	_, err = first.SaveTemporal(context.Background(),
		Memory{ID: "restart", Category: "fact", Content: "reclaim after restart", Source: "test"},
		TemporalWriteOptions{ProcessingJobs: []ProcessingJobSpec{{
			Kind: embeddingJobKind, Generation: "vec4mock",
		}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := first.ClaimProcessingJob(
		context.Background(), embeddingJobKind, "crashed-owner", now, time.Minute,
	)
	if err != nil || claimed == nil {
		t.Fatalf("seed stale claim = (%#v, %v)", claimed, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		store:    reopened,
		embedder: &vec4Embedder{},
		cache:    NewEmbeddingCache(reopened.DB(), logger),
		logger:   logger,
		shutdown: make(chan struct{}),
	}
	defer svc.Close()
	svc.ensureDurableWorkers()
	waitForQueueState(t, reopened, embeddingJobKind, JobDone, 3*time.Second)
}

func waitForQueueState(
	t *testing.T,
	store *SQLiteStore,
	kind string,
	want ProcessingJobState,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var state ProcessingJobState
		err := store.DB().QueryRow(
			`SELECT state FROM memory_processing_jobs WHERE kind = ? ORDER BY created_at DESC LIMIT 1`,
			kind,
		).Scan(&state)
		if err == nil && state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s state = %s, want %s (last error %v)", kind, state, want, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
