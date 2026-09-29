package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func idleTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "idle.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func insertJob(t *testing.T, store *SQLiteStore, id, state, generation string, created time.Time, next, lease *time.Time) {
	t.Helper()
	var nextAt, leaseUntil any
	if next != nil {
		nextAt = next.UnixNano()
	}
	if lease != nil {
		leaseUntil = lease.UnixNano()
	}
	if _, err := store.DB().Exec(`INSERT INTO memory_processing_jobs
		(id, revision_id, memory_id, kind, generation, state, next_attempt_at, lease_until, owner, created_at, updated_at)
		VALUES (?, ?, ?, 'embedding', ?, ?, ?, ?, CASE WHEN ? = 'processing' THEN 'other' END, ?, ?)`,
		id, "rev-"+id, "mem-"+id, generation, state, nextAt, leaseUntil, state, created.UnixNano(), created.UnixNano()); err != nil {
		t.Fatal(err)
	}
}

// The read-only check agrees with what a claim would find.
func TestHasClaimableProcessingJob(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	later := now.Add(time.Hour)
	expired := now.Add(-time.Second)
	cases := []struct {
		name          string
		state, gen    string
		created       time.Time
		next, lease   *time.Time
		generations   []string
		createdBefore time.Time
		want          bool
	}{
		{name: "due pending job", state: "pending", gen: "g1", created: now, want: true},
		{name: "retry not due yet", state: "pending", gen: "g1", created: now, next: &later},
		{name: "another generation", state: "pending", gen: "g2", created: now, generations: []string{"g1"}},
		{name: "served generation", state: "pending", gen: "g1", created: now, generations: []string{"g1"}, want: true},
		{name: "newer than the cutoff", state: "pending", gen: "g1", created: now, createdBefore: now.Add(-time.Minute)},
		{name: "older than the cutoff", state: "pending", gen: "g1", created: now.Add(-2 * time.Minute), createdBefore: now.Add(-time.Minute), want: true},
		{name: "lease still held", state: "processing", gen: "g1", created: now, lease: &later},
		{name: "lease expired", state: "processing", gen: "g1", created: now, lease: &expired, want: true},
		{name: "done", state: "done", gen: "g1", created: now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := idleTestStore(t)
			insertJob(t, store, "j1", tc.state, tc.gen, tc.created, tc.next, tc.lease)
			got, err := store.HasClaimableProcessingJob(ctx, "embedding", tc.generations, tc.createdBefore, now)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("HasClaimableProcessingJob = %v, want %v", got, tc.want)
			}
			if tc.state != "pending" {
				return
			}
			job, err := store.ClaimProcessingJobCreatedBefore(ctx, "embedding", "me", tc.generations, tc.createdBefore, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if (job != nil) != tc.want {
				t.Fatalf("claim found a job = %v, but the check said %v", job != nil, tc.want)
			}
		})
	}
}

func TestHasDeliverableRemoteOutbox(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	insert := func(t *testing.T, store *SQLiteStore, state string, next, lease *time.Time) {
		t.Helper()
		var nextAt, leaseUntil any
		if next != nil {
			nextAt = next.UnixNano()
		}
		if lease != nil {
			leaseUntil = lease.UnixNano()
		}
		if _, err := store.DB().Exec(`INSERT INTO remote_outbox
			(operation_id, memory_id, revision_id, remote, project, payload_hash, payload_snapshot,
			 state, next_attempt_at, lease_until, created_at, updated_at)
			VALUES ('op', 'm', 'r', 'default', 'p', 'h', x'00', ?, ?, ?, ?, ?)`,
			state, nextAt, leaseUntil, now.UnixNano(), now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	later, expired := now.Add(time.Hour), now.Add(-time.Second)
	cases := []struct {
		name        string
		state       string
		next, lease *time.Time
		want        bool
	}{
		{name: "empty"},
		{name: "due", state: "pending", want: true},
		{name: "retry later", state: "pending", next: &later},
		{name: "in flight", state: "processing", lease: &later},
		{name: "lease expired", state: "processing", lease: &expired, want: true},
		{name: "delivered", state: "delivered"},
		{name: "dead letter", state: "dead_letter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := idleTestStore(t)
			if tc.state != "" {
				insert(t, store, tc.state, tc.next, tc.lease)
			}
			got, err := store.HasDeliverableRemoteOutbox(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("HasDeliverableRemoteOutbox = %v, want %v", got, tc.want)
			}
		})
	}
}

// lazyEmbedder is a bowEmbedder that reports whether its model is loaded.
type lazyEmbedder struct {
	*bowEmbedder
	loaded bool
}

func (e *lazyEmbedder) Loaded() bool { return e.loaded }

// A poll in a process whose model is unloaded leaves fresh jobs to the others;
// a wake (a save in this process) and a loaded model claim at once.
func TestClaimCutoff(t *testing.T) {
	cases := []struct {
		name     string
		embedder EmbeddingProvider
		woken    bool
		grace    bool
	}{
		{name: "poll, model unloaded", embedder: &lazyEmbedder{bowEmbedder: &bowEmbedder{}}, grace: true},
		{name: "poll, model loaded", embedder: &lazyEmbedder{bowEmbedder: &bowEmbedder{}, loaded: true}},
		{name: "wake, model unloaded", embedder: &lazyEmbedder{bowEmbedder: &bowEmbedder{}}, woken: true},
		{name: "poll, embedder without lazy loading", embedder: &bowEmbedder{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{embedder: tc.embedder}
			cutoff := s.claimCutoff(tc.woken)
			if cutoff.IsZero() == tc.grace {
				t.Fatalf("cutoff %v, want a grace = %v", cutoff, tc.grace)
			}
			if tc.grace {
				if age := time.Since(cutoff); age < unloadedClaimGrace-time.Second || age > unloadedClaimGrace+time.Second {
					t.Fatalf("cutoff is %v ago, want about %v", age, unloadedClaimGrace)
				}
			}
		})
	}
}

// countingStore counts write-transaction claims.
type countingStore struct {
	*SQLiteStore
	claims int
}

func (c *countingStore) ClaimProcessingJobCreatedBefore(ctx context.Context, kind, owner string, generations []string, createdBefore, now time.Time, lease time.Duration) (*ProcessingJob, error) {
	c.claims++
	return c.SQLiteStore.ClaimProcessingJobCreatedBefore(ctx, kind, owner, generations, createdBefore, now, lease)
}

// An idle worker only reads: with nothing queued, a pass claims nothing.
func TestDrainDurableWork_IdlePassTakesNoWriteLock(t *testing.T) {
	store := &countingStore{SQLiteStore: idleTestStore(t)}
	s := &Service{store: store, embedder: &bowEmbedder{}, workerOwner: "w", logger: discardLogger()}
	for i := 0; i < 5; i++ {
		if busy, worked := s.drainDurableWork(false); busy || worked {
			t.Fatalf("idle pass reported busy=%v worked=%v", busy, worked)
		}
	}
	if store.claims != 0 {
		t.Fatalf("an idle worker ran %d claims", store.claims)
	}
}
