package memory

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func newProcessTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "processes.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestLiveProcesses_PrunesStaleAndDeadRows(t *testing.T) {
	ctx := context.Background()
	store := newProcessTestStore(t)
	host, _ := os.Hostname()
	now := time.Now().UTC()

	register := func(p ProcessInfo) {
		t.Helper()
		if err := store.RegisterProcess(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	register(ProcessInfo{PID: os.Getpid(), Host: host, Version: "0.20.0", Role: "serve", StartedAt: now, HeartbeatAt: now})
	// A pid that does not exist on this host: the process died without
	// unregistering, even though its heartbeat looks fresh.
	register(ProcessInfo{PID: 1<<22 + 7, Host: host, Version: "0.20.0", Role: "serve", StartedAt: now, HeartbeatAt: now})
	// Another host: only the heartbeat can tell.
	register(ProcessInfo{PID: 42, Host: "other-host", Version: "0.20.0", Role: "hub", StartedAt: now, HeartbeatAt: now})
	register(ProcessInfo{PID: 43, Host: "other-host", Version: "0.19.2", Role: "serve",
		StartedAt: now.Add(-time.Hour), HeartbeatAt: now.Add(-10 * time.Minute)})

	live, err := store.LiveProcesses(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, p := range live {
		got = append(got, p.PID)
	}
	sort.Ints(got)
	want := []int{42, os.Getpid()}
	sort.Ints(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("live processes: got %v, want %v", got, want)
	}

	var rows int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM processes").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Errorf("dead and stale rows must be pruned, %d rows remain", rows)
	}
}

func TestProcessesBelow_GatesOnVersion(t *testing.T) {
	procs := []ProcessInfo{
		{PID: 1, Version: "0.19.2"},
		{PID: 2, Version: "0.20.0-dev+g3f43bc4.dirty"},
		{PID: 3, Version: "0.21.0"},
		{PID: 4, Version: "dev"},
		{PID: 5, Version: "v0.20.1"},
	}
	var blocked []int
	for _, p := range ProcessesBelow(procs, "0.20.0") {
		blocked = append(blocked, p.PID)
	}
	if len(blocked) != 2 || blocked[0] != 1 || blocked[1] != 4 {
		t.Errorf("expected the 0.19 binary and the unknown build to block, got %v", blocked)
	}
}

func TestRunProcessRegistration_UnregistersWhenDone(t *testing.T) {
	store := newProcessTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	self := LocalProcess("0.20.0", "serve", "embedder")

	done := make(chan struct{})
	go func() {
		store.RunProcessRegistration(ctx, self)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		live, err := store.LiveProcesses(context.Background(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(live) == 1 && live[0].Role == "serve" && len(live[0].Capabilities) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process never appeared in the registry: %+v", live)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done
	live, err := store.LiveProcesses(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("a process that stops must leave the registry, got %+v", live)
	}
}

// A reader prunes a row whose heartbeat looks stale: after a laptop resumes
// from suspend, a stall, or a failed first registration. The next heartbeat
// must put the process back, keeping its start time.
func TestHeartbeat_ReRegistersAPrunedRow(t *testing.T) {
	ctx := context.Background()
	store := newProcessTestStore(t)
	self := LocalProcess("0.20.0", "serve")
	self.StartedAt = time.Now().Add(-time.Hour).UTC()
	if err := store.RegisterProcess(ctx, self); err != nil {
		t.Fatal(err)
	}
	if err := store.UnregisterProcess(ctx, self.Host, self.PID); err != nil { // pruned
		t.Fatal(err)
	}

	if err := store.heartbeatProcess(ctx, self, time.Now()); err != nil {
		t.Fatal(err)
	}
	live, err := store.LiveProcesses(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("a heartbeat after a prune must re-register the process, got %+v", live)
	}
	if !live[0].StartedAt.Equal(self.StartedAt.Truncate(time.Nanosecond)) {
		t.Errorf("start time must survive: got %v, want %v", live[0].StartedAt, self.StartedAt)
	}

	// A heartbeat on a live row advances heartbeat_at and keeps the original
	// start time.
	later := time.Now().Add(time.Minute)
	if err := store.heartbeatProcess(ctx, self, later); err != nil {
		t.Fatal(err)
	}
	live, _ = store.LiveProcesses(ctx, later)
	if len(live) != 1 || !live[0].StartedAt.Equal(self.StartedAt) {
		t.Fatalf("heartbeat must not reset started_at: %+v", live)
	}
	if !live[0].HeartbeatAt.Equal(later.UTC()) {
		t.Errorf("heartbeat must advance heartbeat_at: got %v, want %v", live[0].HeartbeatAt, later.UTC())
	}
}

// A gate that cannot parse its own threshold must not open: it reports every
// process as a blocker.
func TestProcessesBelow_FailsClosedOnInvalidMinimum(t *testing.T) {
	procs := []ProcessInfo{{PID: 1, Version: "0.21.0"}, {PID: 2, Version: "0.19.2"}}
	if got := ProcessesBelow(procs, "not-a-version"); len(got) != len(procs) {
		t.Errorf("invalid minimum must block everything, got %+v", got)
	}
}
