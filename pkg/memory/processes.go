package memory

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProcessInfo is one long-lived anchored process holding the database open:
// an MCP server, the hub, the dashboard or a maintenance run. Short-lived hook
// invocations do not register.
type ProcessInfo struct {
	PID          int
	Host         string
	Version      string
	Role         string
	Capabilities []string
	StartedAt    time.Time
	HeartbeatAt  time.Time
}

const (
	// processHeartbeatEvery is how often a registered process refreshes its
	// row; a row older than processStaleAfter belongs to a process that died
	// without unregistering (SIGKILL, power loss) and is ignored and pruned.
	processHeartbeatEvery = 30 * time.Second
	processStaleAfter     = 3 * processHeartbeatEvery
)

// LocalProcess describes the calling process for registration.
func LocalProcess(version, role string, capabilities ...string) ProcessInfo {
	host, _ := os.Hostname()
	now := time.Now().UTC()
	return ProcessInfo{
		PID: os.Getpid(), Host: host, Version: version, Role: role,
		Capabilities: capabilities, StartedAt: now, HeartbeatAt: now,
	}
}

// RegisterProcess records p, replacing any stale row a previous process with
// the same pid left behind.
func (s *SQLiteStore) RegisterProcess(ctx context.Context, p ProcessInfo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO processes (host, pid, version, role, capabilities, started_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host, pid) DO UPDATE SET
			version = excluded.version, role = excluded.role,
			capabilities = excluded.capabilities,
			started_at = excluded.started_at, heartbeat_at = excluded.heartbeat_at`,
		p.Host, p.PID, p.Version, p.Role, strings.Join(p.Capabilities, ","),
		p.StartedAt.UTC().UnixNano(), p.HeartbeatAt.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("register process %d: %w", p.PID, err)
	}
	return nil
}

// heartbeatProcess refreshes p's row, recreating it if a reader pruned it
// meanwhile (a laptop resuming from suspend makes every row look stale for up
// to one interval) or the first registration failed. started_at keeps the
// value of the row being refreshed.
func (s *SQLiteStore) heartbeatProcess(ctx context.Context, p ProcessInfo, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO processes (host, pid, version, role, capabilities, started_at, heartbeat_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(host, pid) DO UPDATE SET heartbeat_at = excluded.heartbeat_at`,
		p.Host, p.PID, p.Version, p.Role, strings.Join(p.Capabilities, ","),
		p.StartedAt.UTC().UnixNano(), at.UTC().UnixNano())
	return err
}

// UnregisterProcess removes the row for (host, pid).
func (s *SQLiteStore) UnregisterProcess(ctx context.Context, host string, pid int) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM processes WHERE host = ? AND pid = ?", host, pid)
	return err
}

// RunProcessRegistration registers p and keeps its heartbeat fresh until ctx
// is done, then removes the row. It blocks; run it in a goroutine.
func (s *SQLiteStore) RunProcessRegistration(ctx context.Context, p ProcessInfo) {
	if err := s.RegisterProcess(ctx, p); err != nil {
		s.logger.Warn("process registration failed", "error", err)
	}
	ticker := time.NewTicker(processHeartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// ctx is already cancelled; the removal needs its own deadline.
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = s.UnregisterProcess(cleanup, p.Host, p.PID)
			cancel()
			return
		case <-ticker.C:
			if err := s.heartbeatProcess(ctx, p, time.Now()); err != nil {
				s.logger.Debug("process heartbeat failed", "error", err)
			}
		}
	}
}

// LiveProcesses returns the registered processes that are still alive: a
// fresh heartbeat and, for this host, a pid that still exists. Rows that fail
// either test are pruned on the way.
func (s *SQLiteStore) LiveProcesses(ctx context.Context, now time.Time) ([]ProcessInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT host, pid, version, role, capabilities, started_at, heartbeat_at FROM processes ORDER BY started_at")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	var all []ProcessInfo
	for rows.Next() {
		var p ProcessInfo
		var caps string
		var started, beat int64
		if err := rows.Scan(&p.Host, &p.PID, &p.Version, &p.Role, &caps, &started, &beat); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan process: %w", err)
		}
		if caps != "" {
			p.Capabilities = strings.Split(caps, ",")
		}
		p.StartedAt = time.Unix(0, started).UTC()
		p.HeartbeatAt = time.Unix(0, beat).UTC()
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}

	host, _ := os.Hostname()
	var live []ProcessInfo
	for _, p := range all {
		dead := now.Sub(p.HeartbeatAt) > processStaleAfter || (p.Host == host && !processAlive(p.PID))
		if dead {
			_ = s.UnregisterProcess(ctx, p.Host, p.PID)
			continue
		}
		live = append(live, p)
	}
	return live, nil
}

// ProcessesBelow returns the processes older than minVersion. Irreversible
// steps (activating a new embedding space, dropping a legacy column) must not
// run while one is alive: its old code would keep writing the old shape.
// Versions compare on major.minor.patch; a dev build of 0.20 counts as 0.20.
// An unparseable version counts as older, since its behaviour is unknown, and
// an unparseable minVersion blocks every process: the gate fails closed.
func ProcessesBelow(procs []ProcessInfo, minVersion string) []ProcessInfo {
	want, ok := parseVersionTriple(minVersion)
	if !ok {
		return append([]ProcessInfo(nil), procs...)
	}
	var out []ProcessInfo
	for _, p := range procs {
		got, ok := parseVersionTriple(p.Version)
		if !ok || compareTriples(got, want) < 0 {
			out = append(out, p)
		}
	}
	return out
}

func parseVersionTriple(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func compareTriples(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

type processRegistry interface {
	RunProcessRegistration(ctx context.Context, p ProcessInfo)
	LiveProcesses(ctx context.Context, now time.Time) ([]ProcessInfo, error)
}

// RunProcessRegistration lists this process in the registry until ctx is
// done. A store without a registry makes it a no-op.
func (s *Service) RunProcessRegistration(ctx context.Context, p ProcessInfo) {
	if r, ok := s.store.(processRegistry); ok {
		r.RunProcessRegistration(ctx, p)
	}
}

// LiveProcesses returns the registered anchored processes still alive.
func (s *Service) LiveProcesses(ctx context.Context, now time.Time) ([]ProcessInfo, error) {
	r, ok := s.store.(processRegistry)
	if !ok {
		return nil, nil
	}
	return r.LiveProcesses(ctx, now)
}
