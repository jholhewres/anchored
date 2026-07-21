package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDerivedWorkHealthCountsStatesAndOldestPending(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "health.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	m := Memory{ID: "health-memory", Content: "durable derived work health", Category: "fact", Source: "test"}
	revision, err := store.SaveTemporal(context.Background(), m, TemporalWriteOptions{
		ProcessingJobs: []ProcessingJobSpec{{Kind: "embedding", Generation: "g1"}},
		RemoteOutbox: []RemoteOutboxSpec{{
			OperationID: "op-health",
			Remote:      "team",
			Project:     "project",
			Payload:     []byte(`{"content":"snapshot"}`),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimProcessingJob(context.Background(), "embedding", "worker", now, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim job=%v err=%v", job, err)
	}
	if err := store.CompleteProcessingJob(context.Background(), job.ID, "worker", now); err != nil {
		t.Fatal(err)
	}
	if revision == nil {
		t.Fatal("missing revision")
	}
	health, err := store.DerivedWorkHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.Processing.Counts["done"] != 1 || health.Processing.Counts["pending"] != 0 {
		t.Fatalf("processing=%v", health.Processing.Counts)
	}
	if health.Outbox.Counts["pending"] != 1 || health.Outbox.OldestPending == nil || !health.Outbox.OldestPending.Equal(now) {
		t.Fatalf("outbox=%+v", health.Outbox)
	}
}
