package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/importer"
	"github.com/jholhewres/anchored/pkg/memory"
)

func runImport(args []string) {
	fs := newFlagSet("import")
	configPath := fs.String("config", "", "path to config file")
	force := fs.Bool("force", false, "force re-import even if source unchanged")
	skipEmbeddings := fs.Bool("skip-embeddings", false, "skip embedding backfill after import")
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
	results := importer.RunAll(ctx, sources, store, logger, importer.RunAllOptions{Force: *force})

	fmt.Println("\nImport complete:")
	for _, r := range results {
		fmt.Printf("  %s: found=%d imported=%d skipped=%d errors=%d\n",
			r.Source, r.Found, r.Imported, r.Skipped, r.Errors)
	}

	totalImported := 0
	for _, r := range results {
		totalImported += r.Imported
	}

	var pendingHash int
	memSvc.StoreDB().QueryRow("SELECT COUNT(*) FROM memories WHERE content_hash IS NULL OR content_hash = ''").Scan(&pendingHash)
	if pendingHash > 0 {
		fmt.Printf("\nBackfilling content hashes for %d memories...\n", pendingHash)
		hashed, err := memSvc.BackfillContentHash(ctx)
		if err != nil {
			logger.Warn("content hash backfill failed", "error", err)
		} else {
			fmt.Printf("Content hashes backfilled: %d\n", hashed)
		}
	}

	if *skipEmbeddings {
		fmt.Println("\nEmbedding backfill skipped.")
	} else if totalImported > 0 {
		fmt.Println("\nBackfilling embeddings (single-threaded)...")
		embedded, err := memSvc.BackfillEmbeddings(ctx, 200)
		if err != nil {
			logger.Warn("backfill failed", "error", err)
		} else {
			fmt.Printf("Embeddings backfilled: %d\n", embedded)
		}
	} else {
		var pending int
		memSvc.StoreDB().QueryRow("SELECT COUNT(*) FROM memories WHERE embedding IS NULL OR LENGTH(embedding) = 0").Scan(&pending)
		if pending > 0 {
			fmt.Printf("\nNo new imports, but %d memories pending embeddings. Backfilling...\n", pending)
			embedded, err := memSvc.BackfillEmbeddings(ctx, 200)
			if err != nil {
				logger.Warn("backfill failed", "error", err)
			} else {
				fmt.Printf("Embeddings backfilled: %d\n", embedded)
			}
		}
	}

	if len(cfg.Indexer.Paths) > 0 {
		idx := memory.NewMemoryIndexer(memSvc, cfg.Indexer.Paths, logger)
		for _, p := range cfg.Indexer.Paths {
			if err := idx.IndexNow(p); err != nil {
				logger.Warn("indexer failed", "path", p, "error", err)
			}
		}
		fmt.Println("Indexing complete.")
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
