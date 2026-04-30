package main

import (
	"bufio"
	"context"
	"flag"
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
	"github.com/jholhewres/anchored/pkg/stack"
)

const Version = "0.1.0"

func main() {
	configPath := flag.String("config", "", "path to config file (default: ~/.anchored/config.yaml)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("anchored %s\n", Version)
		os.Exit(0)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if err := config.EnsureDirs(cfg); err != nil {
		slog.Error("failed to create directories", "error", err)
		os.Exit(1)
	}

	logger := slog.Default()

	memSvc, err := memory.NewService(cfg, logger)
	if err != nil {
		slog.Error("failed to initialize memory service", "error", err)
		os.Exit(1)
	}
	defer memSvc.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := serveSTDIO(ctx, memSvc, cfg, logger); err != nil {
		slog.Error("serve error", "error", err)
		os.Exit(1)
	}
}

func serveSTDIO(ctx context.Context, memSvc *memory.Service, cfg *config.Config, logger *slog.Logger) error {
	home, _ := os.UserHomeDir()
	identityPath := home + "/.anchored/identity.md"

	identityLayer := stack.NewIdentityLayer(identityPath, logger, 800)
	identityLayer.Start()
	defer identityLayer.Stop()

	projectLayer := stack.NewProjectLayer(func() string {
		stats, err := memSvc.Stats(context.Background())
		if err != nil || stats.TotalMemories == 0 {
			return ""
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("%d memories across %d projects", stats.TotalMemories, len(stats.ByProject)))
		for proj, count := range stats.ByProject {
			lines = append(lines, fmt.Sprintf("• %s: %d", proj, count))
		}
		return joinLines(lines)
	}, 6*time.Hour)

	onDemandLayer := stack.NewOnDemandLayer(func() string {
		stats, _ := memSvc.Stats(context.Background())
		if stats == nil || stats.TotalMemories == 0 {
			return ""
		}
		var lines []string
		for cat, count := range stats.ByCategory {
			lines = append(lines, fmt.Sprintf("• %s: %d", cat, count))
		}
		return joinLines(lines)
	})

	memoryStack := stack.NewStack(identityLayer, projectLayer, onDemandLayer, cfg.Stack.BudgetBytes, logger)

	kgSvc := kg.New(memSvc.StoreDB(), logger)

	server := mcp.NewServer(memSvc, kgSvc, memoryStack, Version, logger)

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

func joinLines(lines []string) string {
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}
