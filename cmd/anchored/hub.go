package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/jholhewres/anchored/pkg/session"
)

// The hub is the always-on local owner: it serves the dashboard AND keeps the
// live-session state honest by running upkeep workers (ending stale sessions,
// pruning old events) on a ticker — even when no coding tool is connected. The
// per-tool `anchored serve --stdio` processes remain lightweight producers.

const hubUnitName = "anchored-hub"

func runHub(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "install", "enable":
			installHubService(args[1:])
			return
		case "uninstall", "remove", "disable":
			uninstallHubService()
			return
		case "status":
			statusHubService()
			return
		case "serve":
			args = args[1:]
		}
	}
	hubServe(args)
}

func hubServe(args []string) {
	fs2 := newFlagSet("hub serve")
	configPath := fs2.String("config", "", "path to config file")
	addr := fs2.String("addr", "127.0.0.1:17777", "listen address (host:port)")
	allowRemote := fs2.Bool("allow-remote", false, "allow binding to non-loopback interfaces (no built-in auth; pair with --token)")
	writeToken := fs2.String("token", "", "require this bearer secret for every request")
	fs2.Parse(args)

	if host := hostOnly(*addr); host != "" && !isLoopback(host) && !*allowRemote {
		slog.Error("refusing to bind non-loopback address without --allow-remote", "addr", *addr)
		os.Exit(1)
	}
	if *allowRemote && *writeToken == "" {
		slog.Warn("--allow-remote set without --token: anyone who can reach this host can read your memory store")
	}

	_, logger, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	mgr := session.NewManager(svc.StoreDB(), logger)
	api := &dashboardAPI{svc: svc, db: svc.StoreDB(), sessions: mgr, logger: logger}

	assets, err := fs.Sub(dashboardAssets, "dashboard/assets")
	if err != nil {
		slog.Error("dashboard assets", "error", err)
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", api.routes())
	mux.Handle("/", noDirListing(http.FileServer(http.FS(assets))))
	guard := &dashboardGuard{writeToken: *writeToken}
	srv := &http.Server{Handler: guard.wrap(cacheControl(mux)), ReadHeaderTimeout: 10 * time.Second}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "addr", *addr, "error", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "anchored hub → http://%s  (dashboard + upkeep workers, Ctrl+C to stop)\n", listener.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runHubWorkers(ctx, mgr, logger)

	srvDone := make(chan struct{})
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("serve", "error", err)
		}
		close(srvDone)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Fprintf(os.Stderr, "shutting down…\n")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	<-srvDone
}

// runHubWorkers ends stale sessions and prunes old events on a ticker so the
// cockpit's live view stays honest without a connected tool. Best-effort:
// errors are logged, never fatal.
func runHubWorkers(ctx context.Context, mgr *session.Manager, logger *slog.Logger) {
	const (
		staleAfter = 2 * time.Hour      // a session with no activity this long is ended
		eventTTL   = 7 * 24 * time.Hour // session_events older than this are pruned
		tick       = 15 * time.Minute
	)
	upkeep := func() {
		if n, err := mgr.EndStaleSessions(ctx, staleAfter); err != nil {
			logger.Warn("hub: end stale sessions", "error", err)
		} else if n > 0 {
			logger.Info("hub: ended stale sessions", "count", n)
		}
		if n, err := mgr.CleanupOldEvents(ctx, eventTTL); err != nil {
			logger.Warn("hub: cleanup old events", "error", err)
		} else if n > 0 {
			logger.Info("hub: pruned old events", "count", n)
		}
	}
	upkeep() // run once at startup
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			upkeep()
		}
	}
}

// --- systemd --user service management (Linux only; best-effort) ---

func hubUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", hubUnitName+".service"), nil
}

func hubUnit(exe, addr string) string {
	return fmt.Sprintf(`[Unit]
Description=anchored hub (always-on dashboard + session upkeep)
After=network.target

[Service]
Type=simple
ExecStart=%s hub serve --addr %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe, addr)
}

func installHubService(args []string) {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "hub service install is Linux-only (systemd --user). Run `anchored hub serve` directly on this OS, or add it to your login items.")
		return
	}
	fs2 := newFlagSet("hub install")
	addr := fs2.String("addr", "127.0.0.1:17777", "listen address (host:port)")
	_ = fs2.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		slog.Error("locate executable", "error", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	unitPath, err := hubUnitPath()
	if err != nil {
		slog.Error("resolve unit path", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		slog.Error("create unit dir", "error", err)
		os.Exit(1)
	}
	if err := os.WriteFile(unitPath, []byte(hubUnit(exe, *addr)), 0o644); err != nil {
		slog.Error("write unit", "error", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "unit written: %s\n", unitPath)
	if user := os.Getenv("USER"); user != "" {
		runCmd("loginctl", "enable-linger", user)
	}
	runCmd("systemctl", "--user", "daemon-reload")
	runCmd("systemctl", "--user", "enable", "--now", hubUnitName)
	fmt.Fprintf(os.Stderr, "\nanchored hub service enabled.\n  URL:     http://%s\n  manage:  systemctl --user {status|stop|restart|disable} %s\n", *addr, hubUnitName)
}

func uninstallHubService() {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "hub service is Linux-only; nothing to uninstall.")
		return
	}
	runCmd("systemctl", "--user", "disable", "--now", hubUnitName)
	if unitPath, err := hubUnitPath(); err == nil {
		_ = os.Remove(unitPath)
	}
	runCmd("systemctl", "--user", "daemon-reload")
	fmt.Fprintf(os.Stderr, "anchored hub service removed.\n")
}

func statusHubService() {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "hub service is Linux-only.")
		return
	}
	cmd := exec.Command("systemctl", "--user", "status", hubUnitName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
