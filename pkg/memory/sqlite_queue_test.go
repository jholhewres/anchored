package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTemporalSaveCommitsRevisionJobsAndOutboxAtomically(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)

	revision, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "atomic", Source: "test"},
		TemporalWriteOptions{
			ProcessingJobs: []ProcessingJobSpec{
				{Kind: "embedding", Generation: "g1"},
				{Kind: "embedding", Generation: "g1"}, // idempotent replay
				{Kind: "kg", Generation: "extractor-v1"},
			},
			RemoteOutbox: []RemoteOutboxSpec{
				{OperationID: "atomic-op", Remote: "https://memory.example", Project: "p1", Payload: []byte(`{"content":"atomic"}`)},
				{OperationID: "atomic-op", Remote: "https://memory.example", Project: "p1", Payload: []byte(`{"content":"atomic"}`)},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	var currentRevision string
	if err := store.DB().QueryRow(`SELECT current_revision_id FROM memories WHERE id = 'm1'`).
		Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if currentRevision != revision.RevisionID {
		t.Fatalf("current revision = %q, want %q", currentRevision, revision.RevisionID)
	}
	assertTableCount(t, store, "memory_processing_jobs", 2)
	assertTableCount(t, store, "remote_outbox", 1)
}

func TestTemporalSaveRollsBackEverythingWhenDerivedInsertFails(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	if _, err := store.DB().Exec(`CREATE TRIGGER reject_job
		BEFORE INSERT ON memory_processing_jobs
		WHEN NEW.kind = 'fail'
		BEGIN SELECT RAISE(ABORT, 'injected job failure'); END`); err != nil {
		t.Fatal(err)
	}

	_, err := store.SaveTemporal(ctx,
		Memory{ID: "rollback", Category: "fact", Content: "must rollback", Source: "test"},
		TemporalWriteOptions{
			ProcessingJobs: []ProcessingJobSpec{{Kind: "fail"}},
			RemoteOutbox: []RemoteOutboxSpec{{
				Remote: "https://memory.example", Payload: []byte("snapshot"),
			}},
		})
	if err == nil {
		t.Fatal("SaveTemporal succeeded despite injected derived-work failure")
	}
	for table, want := range map[string]int{
		"memories":               0,
		"memory_revisions":       0,
		"memory_processing_jobs": 0,
		"remote_outbox":          0,
	} {
		assertTableCount(t, store, table, want)
	}
}

func TestProcessingJobClaimLeaseOwnerAndDeadLetter(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)
	_, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "queue", Source: "test"},
		TemporalWriteOptions{ProcessingJobs: []ProcessingJobSpec{{
			Kind: "embedding", Generation: "g1", MaxAttempts: 2,
		}}})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan *ProcessingJob, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			<-start
			job, claimErr := store.ClaimProcessingJob(ctx, "embedding", owner, now, time.Minute)
			results <- job
			errs <- claimErr
		}(owner)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var claimed *ProcessingJob
	var claims int
	for job := range results {
		if job != nil {
			claimed = job
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent claims = %d, want 1", claims)
	}
	if job, err := store.ClaimProcessingJob(ctx, "embedding", "early", now.Add(30*time.Second), time.Minute); err != nil || job != nil {
		t.Fatalf("claim before lease expiry = (%#v, %v)", job, err)
	}

	reclaimedAt := now.Add(2 * time.Minute)
	reclaimed, err := store.ClaimProcessingJob(ctx, "embedding", "worker-c", reclaimedAt, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed == nil || reclaimed.ID != claimed.ID || reclaimed.Attempts != 2 {
		t.Fatalf("reclaimed job = %#v", reclaimed)
	}
	if err := store.CompleteProcessingJob(ctx, reclaimed.ID, claimed.Owner, reclaimedAt); !errors.Is(err, ErrClaimOwnerMismatch) {
		t.Fatalf("stale owner completion error = %v", err)
	}
	failed, err := store.FailProcessingJob(ctx, reclaimed.ID, "worker-c", "model unavailable",
		reclaimedAt, reclaimedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != JobFailed {
		t.Fatalf("state after max attempts = %s, want failed", failed.State)
	}
	if err := store.RequeueProcessingJob(ctx, failed.ID, reclaimedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	requeued, err := store.ClaimProcessingJob(ctx, "embedding", "worker-d", reclaimedAt.Add(time.Minute), time.Minute)
	if err != nil || requeued == nil || requeued.Attempts != 1 {
		t.Fatalf("requeued claim = (%#v, %v)", requeued, err)
	}
	if err := store.CompleteProcessingJob(ctx, requeued.ID, "worker-d", reclaimedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProcessingJob(ctx, requeued.ID, "worker-d", reclaimedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
}

func TestRemoteOutboxLeaseRetryDeadLetterAndImmutableEnvelope(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)
	revision, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "remote", Source: "test"},
		TemporalWriteOptions{RemoteOutbox: []RemoteOutboxSpec{{
			OperationID: "op-1", Remote: "https://memory.example", Project: "p1",
			PayloadHash: "hash-1", Payload: []byte("snapshot-1"), MaxAttempts: 2,
		}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE remote_outbox SET payload_snapshot = 'changed'
		WHERE operation_id = 'op-1'`); err == nil {
		t.Fatal("immutable outbox envelope was updated")
	}

	first, err := store.ClaimRemoteOutbox(ctx, "sender-a", now, time.Minute)
	if err != nil || first == nil || first.RevisionID != revision.RevisionID {
		t.Fatalf("first outbox claim = (%#v, %v)", first, err)
	}
	retryAt := OutboxRetryAt(now, first.Attempts, 0)
	pending, err := store.FailRemoteOutbox(ctx, first.OperationID, "sender-a",
		"retryable_http", "503", now, retryAt, false)
	if err != nil || pending.State != OutboxPending {
		t.Fatalf("retry failure = (%#v, %v)", pending, err)
	}
	if item, err := store.ClaimRemoteOutbox(ctx, "early", retryAt.Add(-time.Nanosecond), time.Minute); err != nil || item != nil {
		t.Fatalf("early outbox claim = (%#v, %v)", item, err)
	}
	second, err := store.ClaimRemoteOutbox(ctx, "sender-b", retryAt, time.Minute)
	if err != nil || second == nil || second.Attempts != 2 {
		t.Fatalf("second outbox claim = (%#v, %v)", second, err)
	}
	if err := store.DeliverRemoteOutbox(ctx, second.OperationID, "sender-a", nil, retryAt); !errors.Is(err, ErrClaimOwnerMismatch) {
		t.Fatalf("stale outbox owner error = %v", err)
	}
	dead, err := store.FailRemoteOutbox(ctx, second.OperationID, "sender-b",
		"attempt_limit", "still unavailable", retryAt, retryAt.Add(time.Minute), false)
	if err != nil || dead.State != OutboxDeadLetter {
		t.Fatalf("dead-letter result = (%#v, %v)", dead, err)
	}
	if err := store.RequeueRemoteOutbox(ctx, dead.OperationID, retryAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	deliverable, err := store.ClaimRemoteOutbox(ctx, "sender-c", retryAt.Add(time.Minute), time.Minute)
	if err != nil || deliverable == nil || deliverable.Attempts != 1 {
		t.Fatalf("claim after explicit requeue = (%#v, %v)", deliverable, err)
	}
	if err := store.DeliverRemoteOutbox(ctx, deliverable.OperationID, "sender-c",
		[]byte(`{"remote_id":"r1"}`), retryAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeliverRemoteOutbox(ctx, deliverable.OperationID, "sender-c",
		[]byte("ignored duplicate ack"), retryAt.Add(time.Minute)); err != nil {
		t.Fatalf("idempotent delivery: %v", err)
	}
}

func TestTwoRapidRevisionsKeepDistinctOutboxSnapshotsAndJobs(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)
	var revisionIDs []string
	for i, content := range []string{"first", "second"} {
		revision, err := store.SaveTemporal(ctx,
			Memory{ID: "m1", Category: "fact", Content: content, Source: "test"},
			TemporalWriteOptions{
				ProcessingJobs: []ProcessingJobSpec{{Kind: "embedding", Generation: "g1"}},
				RemoteOutbox: []RemoteOutboxSpec{{
					Remote: "https://memory.example", Project: "p1", Payload: []byte(content),
				}},
			})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		revisionIDs = append(revisionIDs, revision.RevisionID)
	}
	if revisionIDs[0] == revisionIDs[1] {
		t.Fatal("rapid saves reused revision ID")
	}
	assertTableCount(t, store, "memory_processing_jobs", 2)
	assertTableCount(t, store, "remote_outbox", 2)
	rows, err := store.DB().Query(`SELECT revision_id, payload_snapshot FROM remote_outbox ORDER BY created_at, operation_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var revisionID string
		var payload []byte
		if err := rows.Scan(&revisionID, &payload); err != nil {
			t.Fatal(err)
		}
		got[revisionID] = string(payload)
	}
	if got[revisionIDs[0]] != "first" || got[revisionIDs[1]] != "second" {
		t.Fatalf("outbox snapshots = %#v", got)
	}
}

func TestRemoteOutboxDoesNotDeliverNewerRevisionAheadOfRetryingOlderRevision(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)
	var revisions []string
	for _, content := range []string{"older", "newer"} {
		revision, err := store.SaveTemporal(ctx,
			Memory{ID: "ordered", Category: "fact", Content: content, Source: "test"},
			TemporalWriteOptions{RemoteOutbox: []RemoteOutboxSpec{{
				Remote: "remote-a", Project: "project-a", Payload: []byte(content),
			}}})
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, revision.RevisionID)
	}
	older, err := store.ClaimRemoteOutbox(ctx, "sender-a", now, time.Minute)
	if err != nil || older == nil || older.RevisionID != revisions[0] {
		t.Fatalf("older claim = (%#v, %v)", older, err)
	}
	retryAt := now.Add(time.Minute)
	if _, err := store.FailRemoteOutbox(
		ctx, older.OperationID, "sender-a", "retryable_http", "503",
		now, retryAt, false,
	); err != nil {
		t.Fatal(err)
	}
	if item, err := store.ClaimRemoteOutbox(
		ctx, "sender-b", now.Add(time.Second), time.Minute,
	); err != nil || item != nil {
		t.Fatalf("newer revision bypassed retrying older revision: (%#v, %v)", item, err)
	}
	retry, err := store.ClaimRemoteOutbox(ctx, "sender-c", retryAt, time.Minute)
	if err != nil || retry == nil || retry.OperationID != older.OperationID {
		t.Fatalf("older retry claim = (%#v, %v)", retry, err)
	}
	if err := store.DeliverRemoteOutbox(
		ctx, retry.OperationID, "sender-c", nil, retryAt,
	); err != nil {
		t.Fatal(err)
	}
	newer, err := store.ClaimRemoteOutbox(ctx, "sender-d", retryAt, time.Minute)
	if err != nil || newer == nil || newer.RevisionID != revisions[1] {
		t.Fatalf("newer claim after older delivery = (%#v, %v)", newer, err)
	}
}

func TestSoftDeleteCancelsUndeliveredRemoteOutbox(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	store.now = clockAt(instant(1))
	if _, err := store.SaveTemporal(ctx, Memory{
		ID: "cancel-me", Category: "decision", Content: "must stay local after forget",
		Source: "test",
	}, TemporalWriteOptions{RemoteOutbox: []RemoteOutboxSpec{{
		Remote: "https://memory.example.test", Project: "project",
		Payload: []byte(`{"content":"must stay local after forget"}`),
	}}}); err != nil {
		t.Fatal(err)
	}
	store.now = clockAt(instant(2))
	if err := store.SoftDelete(ctx, "cancel-me"); err != nil {
		t.Fatal(err)
	}
	var state OutboxState
	var errorClass string
	if err := store.DB().QueryRow(`
		SELECT state, error_class FROM remote_outbox WHERE memory_id = ?`,
		"cancel-me",
	).Scan(&state, &errorClass); err != nil {
		t.Fatal(err)
	}
	if state != OutboxDeadLetter || errorClass != "local_tombstone" {
		t.Fatalf("cancelled outbox = state %q class %q", state, errorClass)
	}
	var operationID string
	if err := store.DB().QueryRow(`
		SELECT operation_id FROM remote_outbox WHERE memory_id = ?`,
		"cancel-me",
	).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueRemoteOutbox(ctx, operationID, instant(3)); err == nil {
		t.Fatal("local tombstone outbox was requeued")
	}
	item, err := store.ClaimRemoteOutbox(
		ctx, "worker", instant(4), time.Minute,
	)
	if err != nil || item != nil {
		t.Fatalf("claim after soft delete = (%#v, %v), want nil", item, err)
	}
}

func TestOutboxPermanentFailureAndOperationCollision(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	now := instant(1)
	store.now = clockAt(now)
	_, err := store.SaveTemporal(ctx,
		Memory{ID: "m1", Category: "fact", Content: "permanent", Source: "test"},
		TemporalWriteOptions{RemoteOutbox: []RemoteOutboxSpec{{
			OperationID: "permanent-op", Remote: "https://memory.example",
			PayloadHash: "h1", Payload: []byte("one"),
		}}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.ClaimRemoteOutbox(ctx, "sender", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := store.FailRemoteOutbox(ctx, item.OperationID, "sender",
		"permanent_http", "401", now, now, true)
	if err != nil || dead.State != OutboxDeadLetter || dead.Attempts != 1 {
		t.Fatalf("permanent failure = (%#v, %v)", dead, err)
	}

	_, err = store.SaveTemporal(ctx,
		Memory{ID: "m2", Category: "fact", Content: "collision", Source: "test"},
		TemporalWriteOptions{RemoteOutbox: []RemoteOutboxSpec{
			{OperationID: "collision-op", Remote: "https://memory.example", PayloadHash: "same", Payload: []byte("one")},
			{OperationID: "collision-op", Remote: "https://memory.example", PayloadHash: "same", Payload: []byte("two")},
		}})
	if err == nil {
		t.Fatal("conflicting immutable operation ID was accepted")
	}
	if current, getErr := store.Get(ctx, "m2"); getErr != nil || current != nil {
		t.Fatalf("collision save did not roll back current view: (%#v, %v)", current, getErr)
	}
}

func TestOutboxErrorClassificationAndBackoff(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		sameConflict  bool
		transportFail bool
		want          OutboxDisposition
	}{
		{"success", 204, false, false, OutboxDispositionDelivered},
		{"same conflict", 409, true, false, OutboxDispositionDelivered},
		{"different conflict", 409, false, false, OutboxDispositionPermanent},
		{"timeout", 408, false, false, OutboxDispositionRetry},
		{"too early", 425, false, false, OutboxDispositionRetry},
		{"rate limit", 429, false, false, OutboxDispositionRetry},
		{"server error", 503, false, false, OutboxDispositionRetry},
		{"transport", 0, false, true, OutboxDispositionRetry},
		{"auth", 401, false, false, OutboxDispositionPermanent},
		{"unprocessable", 422, false, false, OutboxDispositionPermanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyOutboxResult(tt.status, tt.sameConflict, tt.transportFail); got != tt.want {
				t.Fatalf("classification = %s, want %s", got, tt.want)
			}
		})
	}
	now := instant(1)
	got := OutboxRetryAtFor("op-1", now, 3, 0)
	if got.Before(now.Add(3200*time.Millisecond)) || got.After(now.Add(4800*time.Millisecond)) {
		t.Fatalf("attempt 3 jittered retry = %s", got)
	}
	if again := OutboxRetryAtFor("op-1", now, 3, 0); !again.Equal(got) {
		t.Fatalf("jitter is not deterministic: %s != %s", got, again)
	}
	if other := OutboxRetryAtFor("op-2", now, 3, 0); other.Equal(got) {
		t.Fatalf("different operation IDs received identical jitter: %s", got)
	}
	if got := OutboxRetryAt(now, 1, 2*time.Hour); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("Retry-After cap = %s", got)
	}
}

func TestHardDeletePurgesProcessingJobsAndRemoteOutbox(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	_, err := store.SaveTemporal(ctx,
		Memory{ID: "purge-one", Category: "fact", Content: "erase me", Source: "test"},
		TemporalWriteOptions{
			ProcessingJobs: []ProcessingJobSpec{{Kind: "embedding", Generation: "g1"}},
			RemoteOutbox: []RemoteOutboxSpec{{
				Remote: "remote-a", Project: "project-a", Payload: []byte("secret snapshot"),
			}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "purge-one"); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, store, "memories", 0)
	assertTableCount(t, store, "memory_revisions", 0)
	assertTableCount(t, store, "memory_processing_jobs", 0)
	assertTableCount(t, store, "remote_outbox", 0)
}

func TestHardDeleteByScopePurgesOnlyMatchingDerivedWork(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"purge", "keep"} {
		project := id
		_, err := store.SaveTemporal(ctx,
			Memory{
				ID: id, ProjectID: &project, Category: "fact",
				Content: id, Source: "test",
			},
			TemporalWriteOptions{
				ProcessingJobs: []ProcessingJobSpec{{Kind: "kg", Generation: "g1"}},
				RemoteOutbox: []RemoteOutboxSpec{{
					Remote: "remote-a", Project: project, Payload: []byte(id),
				}},
			})
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.DeleteByScope(ctx, DeleteScopeOptions{
		ProjectID: "purge", Hard: true,
	})
	if err != nil || n != 1 {
		t.Fatalf("hard delete by scope = (%d, %v)", n, err)
	}
	assertTableCount(t, store, "memories", 1)
	assertTableCount(t, store, "memory_revisions", 1)
	assertTableCount(t, store, "memory_processing_jobs", 1)
	assertTableCount(t, store, "remote_outbox", 1)
	var memoryID string
	if err := store.DB().QueryRow(`SELECT memory_id FROM remote_outbox`).Scan(&memoryID); err != nil {
		t.Fatal(err)
	}
	if memoryID != "keep" {
		t.Fatalf("remaining outbox memory = %q, want keep", memoryID)
	}
}

func TestOlderRevisionCannotOverwriteCurrentEmbedding(t *testing.T) {
	store := newTemporalTestStore(t)
	ctx := context.Background()
	first, err := store.SaveTemporal(ctx,
		Memory{ID: "m-current", Category: "fact", Content: "first", Source: "test"},
		TemporalWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.SaveTemporal(ctx,
		Memory{ID: "m-current", Category: "fact", Content: "second", Source: "test"},
		TemporalWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := store.UpdateEmbeddingForRevision(
		ctx, first.MemoryID, first.RevisionID, []float32{1, 1, 1, 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("stale revision updated the current embedding")
	}
	updated, err = store.UpdateEmbeddingForRevision(
		ctx, second.MemoryID, second.RevisionID, []float32{2, 2, 2, 2},
	)
	if err != nil || !updated {
		t.Fatalf("current revision embedding update = (%v, %v)", updated, err)
	}
	current, err := store.Get(ctx, second.MemoryID)
	if err != nil || current == nil {
		t.Fatalf("get current memory = (%#v, %v)", current, err)
	}
	if len(current.Embedding) != 4 || current.Embedding[0] != 2 {
		t.Fatalf("current embedding = %v, want current revision vector", current.Embedding)
	}
}

func assertTableCount(t *testing.T, store *SQLiteStore, table string, want int) {
	t.Helper()
	var got int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
