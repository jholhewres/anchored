package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// dashboardRefreshEvery is how often a serve checks whether the dashboard
// service still runs a binary that an update has replaced.
const dashboardRefreshEvery = 10 * time.Minute

// dashboardSystemctl runs `systemctl --user` and returns its stdout (tests
// swap it).
var dashboardSystemctl = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output()
}

// dashboardProcRoot is where the service's process is inspected (tests swap it).
var dashboardProcRoot = "/proc"

// restartStaleDashboard restarts the anchored-dashboard user service when its
// process runs a binary that has since been replaced at selfExe. An update
// swaps the file in place, but a service keeps executing the old code until
// something restarts it. An old dashboard also holds the database, so it holds
// every upgrade step that waits for older processes to exit. A dashboard started
// from another path (a dev build, a second install) is left alone. It reports
// whether a restart was requested.
func restartStaleDashboard(ctx context.Context, selfExe string, logger *slog.Logger) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	unit := dashboardUnitName + ".service"
	out, err := dashboardSystemctl(ctx, "show", "--property=MainPID", "--value", unit)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return false
	}
	exe, err := os.Readlink(filepath.Join(dashboardProcRoot, strconv.Itoa(pid), "exe"))
	if err != nil || !strings.HasSuffix(exe, " (deleted)") {
		return false
	}
	if strings.TrimSuffix(exe, " (deleted)") != selfExe {
		return false
	}
	// The replacement has to be on disk, or the restart would leave the
	// service down.
	if _, err := os.Stat(selfExe); err != nil {
		return false
	}
	if _, err := dashboardSystemctl(ctx, "try-restart", "--no-block", unit); err != nil {
		logger.Warn("dashboard service runs a replaced binary and could not be restarted", "pid", pid, "error", err)
		return false
	}
	logger.Info("restarted the dashboard service to load the updated binary", "pid", pid)
	return true
}

// runDashboardRefresh checks at startup and then every interval, so the
// dashboard picks up an update without waiting for a reboot.
func runDashboardRefresh(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	exe := memory.CurrentExecutable()
	check := func() {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		restartStaleDashboard(c, exe, logger)
	}
	check()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}
