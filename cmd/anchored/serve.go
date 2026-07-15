package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/debuglog"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"
	"github.com/jholhewres/anchored/pkg/updater"
)

func runServe() {
	logger := newServeLogger()

	cfg, err := loadConfig("")
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if err := config.EnsureDirs(cfg); err != nil {
		slog.Error("failed to create directories", "error", err)
		os.Exit(1)
	}

	memSvc, err := memory.NewService(cfg, logger)
	if err != nil {
		slog.Error("failed to initialize memory service", "error", err)
		os.Exit(1)
	}
	defer memSvc.Close()

	indexer := memory.NewMemoryIndexer(memSvc, cfg.Indexer.Paths, logger)
	if cfg.Indexer.Interval != "" {
		if d, err := time.ParseDuration(cfg.Indexer.Interval); err == nil {
			indexer.SetInterval(d)
		}
	}
	if cfg.Indexer.Enabled {
		indexer.Start()
		defer indexer.Stop()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Lifecycle guards. Historically the server only stopped on stdin EOF, so
	// when an MCP client (Claude Code, Cursor, ...) exited abnormally — crash,
	// SIGKILL, or a lingering parent still holding the pipe — EOF never arrived
	// and the process leaked. Each orphan pins the ~450MB ONNX model resident,
	// which is how a handful of closed IDE windows ballooned into GBs of RAM.
	// watchParent triggers shutdown when our client goes away; forceExit is the
	// backstop that guarantees the process actually terminates afterwards.
	go watchParent(ctx, cancel, logger)
	go forceExitAfterShutdown(ctx, logger)

	go updater.Run(ctx, updater.Options{
		CurrentVersion: Version,
		Logger:         logger,
	})

	// session_events grows by ~1 row per tool call (PostToolUse hook). Without
	// retention the table balloons in long-lived projects. We sweep on serve
	// startup and once a day after that; default 30-day window keeps recap
	// queries fast while preserving a useful historical horizon.
	go runEventCleanup(ctx, memSvc, logger, 30*24*time.Hour, 24*time.Hour)

	if cfg.Curation.Enabled {
		go runCurationWorker(ctx, memSvc, cfg.Curation, logger)
	} else {
		logger.Info("curation worker disabled")
	}

	if err := serveSTDIO(ctx, memSvc, cfg, logger); err != nil {
		slog.Error("serve error", "error", err)
		os.Exit(1)
	}
}

// newServeLogger builds the logger for the long-running MCP server. It writes
// to stderr (stdout is reserved for the JSON-RPC protocol over STDIO) but
// defaults to WARN: MCP clients such as Cursor surface every stderr line as an
// "[error]", so routine operational INFO (vector cache loaded, ONNX init,
// recurring eviction cycles) used to flood the client's error pane with false
// alarms. Warnings and errors still pass through. Set ANCHORED_LOG_LEVEL to
// debug|info|warn|error to override (e.g. when diagnosing the server itself).
func newServeLogger() *slog.Logger {
	level := slog.LevelWarn
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ANCHORED_LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func serveSTDIO(ctx context.Context, memSvc *memory.Service, cfg *config.Config, logFn *slog.Logger) error {
	kgSvc := kg.New(memSvc.StoreDB(), logFn)
	memSvc.SetKGExtractor(kg.NewPatternExtractor(kgSvc, logFn))

	sessionMgr := session.NewManager(memSvc.StoreDB(), logFn)

	var optimizer mcp.OptimizerFacade
	if cfg.ContextOptimizer.Enabled {
		opt, err := mcp.NewCtxOptimizer(memSvc.StoreDB(), cfg.ContextOptimizer, logFn)
		if err != nil {
			logFn.Warn("context optimizer init failed, ctx_* tools unavailable", "error", err)
		} else {
			optimizer = opt
		}
	}
	if optimizer != nil {
		defer optimizer.Close()
	}

	server := mcp.NewServer(memSvc, kgSvc, sessionMgr, optimizer, cfg, Version, logFn)

	// Optional NDJSON event log for post-mortem analysis. No-op when
	// debug.enabled is false (the default) and ANCHORED_DEBUG isn't set.
	dlog := debuglog.Open(cfg)
	defer dlog.Close()
	if dlog.Enabled() {
		logFn.Info("anchored debug log enabled", "path", dlog.Path())
		dlog.Event("server.start", map[string]any{"version": Version})
	}
	server.SetDebugLogger(dlog)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// The blocking stdin read cannot be interrupted by context cancellation
	// (closing os.Stdin does not unblock an in-flight read on fd 0), so we run
	// the scanner in its own goroutine and feed decoded lines over a channel.
	// The main loop then selects between incoming lines and ctx.Done(), letting
	// a signal or parent-death cancellation return immediately and run the
	// deferred cleanup. The reader goroutine is abandoned on shutdown; it is
	// reaped when the process exits.
	lines := make(chan []byte)
	scanErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			b := scanner.Bytes()
			buf := make([]byte, len(b)) // scanner reuses its buffer; copy before handing off
			copy(buf, b)
			select {
			case lines <- buf:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			scanErr <- err
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Signal or parent death: stop serving and fall through to cleanup.
			goto shutdown
		case line, ok := <-lines:
			if !ok {
				// stdin EOF — the client closed the pipe. Surface a genuine read
				// error, but treat a cancelled context as an expected shutdown.
				if err := drainScanErr(scanErr); err != nil && ctx.Err() == nil {
					return fmt.Errorf("stdin read: %w", err)
				}
				goto shutdown
			}
			if len(line) == 0 {
				continue
			}
			if response := server.HandleMessage(ctx, line); response != nil {
				fmt.Printf("%s\n", response)
			}
		}
	}

shutdown:

	if sessionMgr != nil {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessionMgr.EndStaleSessions(ctx2, 30*time.Minute)
	}

	return nil
}

// drainScanErr returns the reader goroutine's scan error if one has already
// been reported, without blocking when none is pending.
func drainScanErr(scanErr <-chan error) error {
	select {
	case err := <-scanErr:
		return err
	default:
		return nil
	}
}

// runEventCleanup periodically deletes session_events rows older than
// retention. Runs once on startup, then every interval. Cancellation via ctx
// stops cleanly. Errors are logged at Warn but never block — the goroutine
// keeps trying on the next tick.
func runEventCleanup(ctx context.Context, memSvc *memory.Service, logger *slog.Logger, retention, interval time.Duration) {
	mgr := session.NewManager(memSvc.StoreDB(), logger)
	sweep := func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := mgr.CleanupOldEvents(c, retention)
		if err != nil {
			logger.Warn("session_events cleanup failed", "error", err)
			return
		}
		if n > 0 {
			logger.Info("session_events cleanup", "deleted", n, "retention_hours", int(retention.Hours()))
		}
	}
	sweep()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

const (
	// parentPollInterval is how often the parent-death watchdog re-checks the
	// parent PID. 5s frees an orphaned server's memory within seconds while the
	// getppid syscall cost stays negligible.
	parentPollInterval = 5 * time.Second
	// shutdownGrace bounds graceful teardown after a shutdown signal. If Close()
	// (embedder + store) has not returned by then we force-exit so the process
	// never lingers.
	shutdownGrace = 10 * time.Second
)

// watchParent shuts the server down when the process that launched it exits.
// MCP clients speak over stdin/stdout, so a departed client normally closes
// stdin and we see EOF — but a crashed or lingering parent can leave the pipe
// open, and the server would otherwise run forever holding the ONNX model in
// RAM. We capture the parent PID at startup and cancel once we are reparented
// (getppid changes, typically to 1 or a subreaper). setParentDeathSignal adds
// an immediate, kernel-level notification on Linux. Set ANCHORED_NO_PARENT_WATCH
// to any non-empty value to disable this watchdog.
func watchParent(ctx context.Context, cancel context.CancelFunc, logger *slog.Logger) {
	if os.Getenv("ANCHORED_NO_PARENT_WATCH") != "" {
		return
	}
	setParentDeathSignal(logger)
	watchParentPoll(ctx, cancel, logger, os.Getppid, parentPollInterval)
}

// watchParentPoll is the testable core of watchParent: it captures the parent
// PID via getppid and cancels once it changes (the parent exited and we were
// reparented). getppid/interval are injected so the reparent transition can be
// exercised without a real fork.
func watchParentPoll(ctx context.Context, cancel context.CancelFunc, logger *slog.Logger, getppid func() int, interval time.Duration) {
	orig := getppid()
	if orig <= 1 {
		// Already orphaned or running without a meaningful parent (e.g. launched
		// directly under init). Nothing reliable to watch.
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ppid := getppid(); ppid != orig {
				logger.Info("parent process exited, shutting down", "orig_ppid", orig, "new_ppid", ppid)
				cancel()
				return
			}
		}
	}
}

// forceExitAfterShutdown guarantees a shutdown signal actually terminates the
// process. serveSTDIO closes stdin on cancellation to unblock its read and let
// deferred cleanup run; if that cleanup stalls (e.g. a wedged background embed
// blocking Service.Close's wg.Wait), we exit anyway once the grace window ends.
func forceExitAfterShutdown(ctx context.Context, logger *slog.Logger) {
	<-ctx.Done()
	timer := time.NewTimer(shutdownGrace)
	defer timer.Stop()
	<-timer.C
	logger.Warn("graceful shutdown exceeded grace period, forcing exit", "grace", shutdownGrace)
	os.Exit(0)
}

func loadConfig(explicit string) (*config.Config, error) {
	if explicit != "" {
		return config.Load(explicit)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return config.Defaults(), nil
	}

	return config.Load(home + "/.anchored/config.yaml")
}
