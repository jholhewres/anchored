package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDashboard stands in for systemd and /proc: the service's MainPID and the
// executable link of that process.
type fakeDashboard struct {
	mainPID string
	showErr error
	calls   []string
}

func (f *fakeDashboard) install(t *testing.T, exeLink string) {
	t.Helper()
	root := t.TempDir()
	if exeLink != "" {
		dir := filepath.Join(root, f.mainPID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exeLink, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	oldRoot, oldCtl := dashboardProcRoot, dashboardSystemctl
	dashboardProcRoot = root
	dashboardSystemctl = func(_ context.Context, args ...string) ([]byte, error) {
		f.calls = append(f.calls, strings.Join(args, " "))
		if args[0] == "show" {
			return []byte(f.mainPID + "\n"), f.showErr
		}
		return nil, nil
	}
	t.Cleanup(func() { dashboardProcRoot, dashboardSystemctl = oldRoot, oldCtl })
}

func (f *fakeDashboard) restarted() bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, "try-restart") {
			return true
		}
	}
	return false
}

func newBinary(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "anchored")
	if err := os.WriteFile(p, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRestartStaleDashboard(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the dashboard service is a systemd --user unit")
	}
	quiet := slog.New(slog.DiscardHandler)
	self := newBinary(t)

	cases := []struct {
		name    string
		mainPID string
		showErr error
		exeLink string
		selfExe string
		want    bool
	}{
		{name: "binary replaced at our path", mainPID: "4242", exeLink: self + " (deleted)", selfExe: self, want: true},
		{name: "already runs the current binary", mainPID: "4242", exeLink: self, selfExe: self},
		{name: "started from another install", mainPID: "4242", exeLink: "/opt/other/anchored (deleted)", selfExe: self},
		// MainPID 0 means the service is not running, whatever /proc holds.
		{name: "service not running", mainPID: "0", exeLink: self + " (deleted)", selfExe: self},
		{name: "no user systemd", mainPID: "", showErr: errors.New("no bus"), selfExe: self},
		{name: "replacement missing on disk", mainPID: "4242", exeLink: "/gone/anchored (deleted)", selfExe: "/gone/anchored"},
		{name: "own path unknown", mainPID: "4242", exeLink: self + " (deleted)", selfExe: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDashboard{mainPID: tc.mainPID, showErr: tc.showErr}
			f.install(t, tc.exeLink)
			got := restartStaleDashboard(context.Background(), tc.selfExe, quiet)
			if got != tc.want || f.restarted() != tc.want {
				t.Fatalf("restart = %v (systemctl calls %q), want %v", got, f.calls, tc.want)
			}
		})
	}
}

// The restart goes through try-restart --no-block: it never starts a service
// the user stopped, and a serve does not wait for the dashboard to come back.
func TestRestartStaleDashboard_UsesTryRestartWithoutBlocking(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the dashboard service is a systemd --user unit")
	}
	self := newBinary(t)
	f := &fakeDashboard{mainPID: "4242"}
	f.install(t, self+" (deleted)")
	restartStaleDashboard(context.Background(), self, slog.New(slog.DiscardHandler))
	want := "try-restart --no-block " + dashboardUnitName + ".service"
	if len(f.calls) != 2 || f.calls[1] != want {
		t.Fatalf("systemctl calls %q, want show then %q", f.calls, want)
	}
}
