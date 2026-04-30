package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/importer"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/mcp"
	"github.com/jholhewres/anchored/pkg/stack"
)

const Version = "0.1.1"

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintf(os.Stderr, "anchored %s — persistent cross-tool memory for AI coding agents\n\n", Version)
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  anchored                  Start MCP server (STDIO)\n")
		fmt.Fprintf(os.Stderr, "  anchored import [sources] Import memories from detected sources\n")
		fmt.Fprintf(os.Stderr, "  anchored --version        Print version\n")
		fmt.Fprintf(os.Stderr, "\nImport sources: claude-code devclaw opencode cursor all\n")
		os.Exit(0)
	}

	switch os.Args[1] {
	case "import":
		runImport(os.Args[2:])
	default:
		runServe()
	}
}

func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	selected := fs.Args()
	if len(selected) == 0 {
		selected = []string{"all"}
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	memSvc, err := memory.NewService(cfg, logger)
	if err != nil {
		slog.Error("failed to initialize memory service", "error", err)
		os.Exit(1)
	}
	defer memSvc.Close()

	home, _ := os.UserHomeDir()
	logFn := func(msg string, args ...any) { logger.Info(msg, args...) }
	sources := buildSources(home, selected, logFn)

	if len(sources) == 0 {
		fmt.Println("No sources found or selected.")
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	store := &serviceStoreAdapter{svc: memSvc}
	results := importer.RunAll(ctx, sources, store, logger)

	fmt.Println("\nImport complete:")
	for _, r := range results {
		fmt.Printf("  %s: found=%d imported=%d skipped=%d errors=%d\n",
			r.Source, r.Found, r.Imported, r.Skipped, r.Errors)
	}
}

func buildSources(home string, selected []string, logFn func(string, ...any)) []importer.Source {
	wantAll := len(selected) == 1 && selected[0] == "all"
	want := make(map[string]bool)
	for _, s := range selected {
		want[s] = true
	}

	var sources []importer.Source

	if wantAll || want["claude-code"] {
		src := importer.NewClaudeCodeImporter(home+"/.claude/projects", logFn)
		if src.Detect() {
			sources = append(sources, src)
		}
	}

	if wantAll || want["devclaw"] {
		paths := []string{
			home + "/Workspace/private/devclaw/data/memory.db",
			home + "/.devclaw/data/memory.db",
			home + "/.config/devclaw/data/memory.db",
		}
		for _, p := range paths {
			src := importer.NewDevClawImporter(p, logFn)
			if src.Detect() {
				sources = append(sources, src)
				break
			}
		}
	}

	if wantAll || want["opencode"] {
		src := importer.NewOpenCodeImporter(home, logFn)
		if src.Detect() {
			sources = append(sources, src)
		}
	}

	if wantAll || want["cursor"] {
		src := importer.NewCursorImporter(home+"/.cursor", logFn)
		if src.Detect() {
			sources = append(sources, src)
		}
	}

	return sources
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config file (default: ~/.anchored/config.yaml)")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Parse(os.Args[1:])

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

type serviceStoreAdapter struct {
	svc *memory.Service
}

func (a *serviceStoreAdapter) SaveMemory(ctx context.Context, content, category, source string, cwd string) error {
	return a.svc.SaveMemory(ctx, content, category, source, cwd)
}

func serveSTDIO(ctx context.Context, memSvc *memory.Service, cfg *config.Config, logFn *slog.Logger) error {
	home, _ := os.UserHomeDir()
	identityPath := home + "/.anchored/identity.md"

	identityLayer := stack.NewIdentityLayer(identityPath, logFn, 800)
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
		return strings.Join(lines, "\n")
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
		return strings.Join(lines, "\n")
	})

	memoryStack := stack.NewStack(identityLayer, projectLayer, onDemandLayer, cfg.Stack.BudgetBytes, logFn)

	kgSvc := kg.New(memSvc.StoreDB(), logFn)

	server := mcp.NewServer(memSvc, kgSvc, memoryStack, Version, logFn)

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
