package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/session"
)

func runServe() {
	logger := slog.Default()

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

	if err := serveSTDIO(ctx, memSvc, cfg, logger); err != nil {
		slog.Error("serve error", "error", err)
		os.Exit(1)
	}
}

func serveSTDIO(ctx context.Context, memSvc *memory.Service, cfg *config.Config, logFn *slog.Logger) error {
	kgSvc := kg.New(memSvc.StoreDB(), logFn)
	memSvc.SetKGExtractor(kg.NewPatternExtractor(kgSvc, logFn))

	sessionMgr := session.NewManager(memSvc.StoreDB(), logFn)

	server := mcp.NewServer(memSvc, kgSvc, sessionMgr, Version, logFn)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		response := server.HandleMessage(ctx, line)
		if response == nil {
			continue
		}

		fmt.Printf("%s\n", response)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("stdin read: %w", err)
	}

	if sessionMgr != nil {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		sessionMgr.EndStaleSessions(ctx2, 30*time.Minute)
	}

	return nil
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
