package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// dashboardAssets embeds the SPA (HTML/JS/CSS + vendored Chart.js and
// vis-network) so the binary is fully self-contained and works offline.
//
//go:embed dashboard/assets
var dashboardAssets embed.FS

// runDashboard starts a local-only HTTP server that visualizes the anchored
// SQLite store. It is read-mostly; the only write path is soft-deleting a
// single memory (same semantics as `anchored forget`).
//
// Sub-actions: `anchored dashboard install|uninstall|status` manage a systemd
// user service so the dashboard stays up across reboots without a terminal.
func runDashboard(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "install", "enable":
			installDashboardService(args[1:])
			return
		case "uninstall", "remove", "disable":
			uninstallDashboardService()
			return
		case "status":
			statusDashboardService()
			return
		}
	}

	flagSet := newFlagSet("dashboard")
	configPath := flagSet.String("config", "", "path to config file")
	addr := flagSet.String("addr", "127.0.0.1:17777", "listen address (host:port)")
	noOpen := flagSet.Bool("no-open", false, "do not open the browser automatically")
	allowRemote := flagSet.Bool("allow-remote", false, "allow binding to non-loopback interfaces (DANGEROUS: the dashboard has no built-in auth; pair with --token)")
	writeToken := flagSet.String("token", "", "require this bearer secret for every request (reads included); recommended with --allow-remote")
	flagSet.Parse(args)

	// The dashboard has no authentication layer, so by default it must only bind
	// a loopback address — otherwise it silently exposes the whole memory store
	// (and its write path) to the local network. --allow-remote is the explicit,
	// loud opt-out.
	if host := hostOnly(*addr); host != "" && !isLoopback(host) && !*allowRemote {
		slog.Error("refusing to bind non-loopback address without --allow-remote",
			"addr", *addr,
			"hint", "the dashboard has no auth; binding here would expose your memory store to the network")
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

	api := &dashboardAPI{svc: svc, db: svc.StoreDB(), logger: logger}

	assets, err := fs.Sub(dashboardAssets, "dashboard/assets")
	if err != nil {
		slog.Error("dashboard assets", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", api.routes())                               // JSON endpoints
	mux.Handle("/", noDirListing(http.FileServer(http.FS(assets)))) // SPA shell + static assets

	guard := &dashboardGuard{writeToken: *writeToken}

	srv := &http.Server{
		Handler:           guard.wrap(cacheControl(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		slog.Error("listen", "addr", *addr, "error", err)
		os.Exit(1)
	}
	baseURL := "http://" + listener.Addr().String()
	// When a write token is configured, the browser session needs to carry it:
	// open the app at ?token=… so the guard can mint a cookie the SPA sends on
	// every subsequent fetch.
	openURL := baseURL
	if *writeToken != "" {
		openURL = baseURL + "/?token=" + *writeToken
	}
	fmt.Fprintf(os.Stderr, "anchored dashboard → %s  (Ctrl+C to stop)\n", baseURL)

	srvDone := make(chan struct{})
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("serve", "error", err)
		}
		close(srvDone)
	}()

	if !*noOpen {
		go openBrowser(openURL)
	}

	// Block until interrupted, then shut down cleanly so WAL checkpoints and
	// in-flight writes settle before the process exits.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Fprintf(os.Stderr, "shutting down…\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	<-srvDone
}

// noDirListing suppresses the FileServer's built-in directory listing. The
// embedded asset tree has no secret files (only the SPA shell and vendored
// public libs), but an open listing needlessly advertises the layout; requests
// for a trailing-slash path other than the app root (which resolves to
// index.html) get a 404 instead of an auto-generated file index.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") && r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cacheControl sets cache headers so the browser always revalidates app assets
// (index.html / app.js / styles.css change with every binary rebuild) while
// still caching the large vendored libs for an hour. Without this, browsers
// heuristically cache a stale app.js and ignore dashboard updates.
func cacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		} else {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			w.Header().Set("Pragma", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

// dashboardGuard layers the security posture of a local-only, unauthenticated
// dashboard on top of the routing mux:
//
//   - DNS-rebinding defence: every request's Host header must resolve to a
//     loopback address, so a malicious site that rebinds its domain to
//     127.0.0.1 can't reach the API. (The dashboard serves no traffic at all
//     to a non-local Host.)
//   - CSRF defence: state-changing methods (anything that isn't a safe read)
//     are rejected when an Origin/Referer header names a non-loopback origin.
//     Same-origin browser writes and curl carry no such header and pass.
//   - Optional bearer token: when --token is set, every request (reads
//     included) requires a matching Bearer header or the anchored_dash cookie
//     (minted from ?token= on first load), so pairing --token with
//     --allow-remote keeps the whole corpus private.
//   - Hardening headers: CSP, nosniff, no-referrer, no framing.
type dashboardGuard struct {
	writeToken string // empty = no token required on writes
}

const dashCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';" +
	" img-src 'self' data: blob:; connect-src 'self'; font-src 'self' data:;" +
	" object-src 'none'; base-uri 'none'; frame-ancestors 'none'"

// hostOnly strips the port from a host[:port] string, tolerating IPv6 literals
// like [::1]:17777. An empty input returns "".
func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return strings.Trim(h, "[]")
	}
	return strings.Trim(hp, "[]")
}

// isLoopback reports whether host is a loopback address or "localhost".
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (g *dashboardGuard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS-rebinding guard: a rebinding attack always carries the attacker's
		// domain as Host, never empty, so only block when a non-local Host is
		// present (keeps HTTP/1.0 / Host-less clients working).
		if r.Host != "" && !isLoopback(hostOnly(r.Host)) {
			writeErr(w, http.StatusForbidden, "forbidden: non-local Host")
			return
		}

		// Bootstrap a cookie from ?token= so the browser session authenticates
		// subsequent write fetches when a write token is configured.
		if g.writeToken != "" {
			if t := r.URL.Query().Get("token"); t != "" {
				if subtle.ConstantTimeCompare([]byte(t), []byte(g.writeToken)) == 1 {
					http.SetCookie(w, &http.Cookie{
						Name: "anchored_dash", Value: t, Path: "/", HttpOnly: true,
						SameSite: http.SameSiteLaxMode,
					})
					clean := *r.URL
					q := clean.Query()
					q.Del("token")
					clean.RawQuery = q.Encode()
					http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
					return
				}
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}

		w.Header().Set("Content-Security-Policy", dashCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// With --token configured, gate EVERYTHING — reads included — on the
		// bearer header or anchored_dash cookie, so the corpus can't be read
		// without the secret. This matters when --token is paired with
		// --allow-remote; locally (no token) it's a no-op. The ?token= bootstrap
		// above has already minted the cookie before we reach here.
		if g.writeToken != "" && !g.authed(r) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// CSRF: state-changing methods must additionally not be driven from a
		// non-loopback origin (same-origin browser writes and curl carry no
		// such header and pass).
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !g.allowOrigin(r) {
				writeErr(w, http.StatusForbidden, "forbidden: cross-origin write blocked")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// allowOrigin blocks state-changing requests that carry an Origin or Referer
// naming a non-loopback host (the CSRF signal a browser always sends on
// cross-origin writes). Absent headers — same-origin browser writes and curl —
// are allowed.
func (g *dashboardGuard) allowOrigin(r *http.Request) bool {
	for _, hv := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		if hv == "" {
			continue
		}
		u, err := url.Parse(hv)
		if err != nil {
			return false
		}
		if !isLoopback(hostOnly(u.Host)) {
			return false
		}
	}
	return true
}

// authed accepts either a Bearer Authorization header (scripts) or the
// anchored_dash cookie (browser session bootstrapped via ?token=).
func (g *dashboardGuard) authed(r *http.Request) bool {
	want := []byte(g.writeToken)
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(ah, "Bearer ")), want) == 1 {
			return true
		}
	}
	if c, err := r.Cookie("anchored_dash"); err == nil {
		if subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
			return true
		}
	}
	return false
}

// openBrowser launches the user's default browser. Best-effort: failure is
// silent (the URL is already printed, so the user can open it manually).
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default: // linux, *bsd
		cmd = exec.Command("xdg-open", url)
	}
	if cmd == nil {
		return
	}
	_ = cmd.Start()
}

// --- systemd --user service management (Linux only; best-effort) ---

const dashboardUnitName = "anchored-dashboard"

func dashboardUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", dashboardUnitName+".service"), nil
}

func dashboardUnit(exe, addr string) string {
	return fmt.Sprintf(`[Unit]
Description=anchored dashboard (local memory viewer)
After=network.target

[Service]
Type=simple
ExecStart=%s dashboard --no-open --addr %s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe, addr)
}

// runCmd runs a command streaming output to stderr. Failure is non-fatal —
// service management must degrade gracefully across distros/environments.
func runCmd(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  (warning) %s: %v\n", name, err)
		return false
	}
	return true
}

// installDashboardService writes a systemd --user unit pointing at the current
// binary, enables lingering (so it survives logout / starts at boot), and
// enables+starts the service. The listen address comes from --addr so an
// install matches the port a user runs interactively.
func installDashboardService(args []string) {
	fs := newFlagSet("dashboard install")
	addr := fs.String("addr", "127.0.0.1:17777", "listen address (host:port)")
	_ = fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		slog.Error("locate executable", "error", err)
		os.Exit(1)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	unitPath, err := dashboardUnitPath()
	if err != nil {
		slog.Error("resolve unit path", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		slog.Error("create unit dir", "error", err)
		os.Exit(1)
	}
	if err := os.WriteFile(unitPath, []byte(dashboardUnit(exe, *addr)), 0o644); err != nil {
		slog.Error("write unit", "error", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "unit written: %s\n", unitPath)

	// Lingering lets the user service run after logout and start at boot;
	// harmless if already enabled or if loginctl is missing.
	if user := os.Getenv("USER"); user != "" {
		runCmd("loginctl", "enable-linger", user)
	}
	runCmd("systemctl", "--user", "daemon-reload")
	runCmd("systemctl", "--user", "enable", "--now", dashboardUnitName)

	fmt.Fprintf(os.Stderr, "\nanchored dashboard service enabled.\n")
	fmt.Fprintf(os.Stderr, "  URL:     http://%s\n", *addr)
	fmt.Fprintf(os.Stderr, "  logs:    journalctl --user -u %s -f\n", dashboardUnitName)
	fmt.Fprintf(os.Stderr, "  manage:  systemctl --user {status|stop|restart|disable} %s\n", dashboardUnitName)
	fmt.Fprintf(os.Stderr, "  remove:  anchored dashboard uninstall\n")
}

// uninstallDashboardService stops+disables the service and removes the unit.
func uninstallDashboardService() {
	runCmd("systemctl", "--user", "disable", "--now", dashboardUnitName)
	if unitPath, err := dashboardUnitPath(); err == nil {
		_ = os.Remove(unitPath)
	}
	runCmd("systemctl", "--user", "daemon-reload")
	fmt.Fprintf(os.Stderr, "anchored dashboard service removed.\n")
}

// statusDashboardService proxies to `systemctl --user status`, forwarding
// systemctl's exit code so the result composes in scripts (e.g. a non-running
// service yields systemctl's exit 3).
func statusDashboardService() {
	cmd := exec.Command("systemctl", "--user", "status", dashboardUnitName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
	if ps := cmd.ProcessState; ps != nil {
		os.Exit(ps.ExitCode())
	}
}
