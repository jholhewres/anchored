package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

// runBackfillEmbeddings is the standalone, observable counterpart to the
// serve-time background worker: it embeds every memory still missing a vector
// and prints progress to stdout, then exits. Useful as a manual one-shot
// (`anchored backfill`) for a large historical store, for cron, or for
// debugging — it does not depend on the MCP server being up.
//
// It is the same idempotent drain (ListWithoutEmbedding), so it is safe to run
// alongside or repeatedly; a fully-embedded store finishes immediately.
// normalizedHashBatch caps one backfill pass. Stamping is a read plus a hash
// per row, so the bound is about keeping a single run's transaction short on a
// multi-GB store, not about CPU.
const normalizedHashBatch = 20000

func runBackfillEmbeddings(args []string) {
	fs := newFlagSet("backfill")
	// `maintenance run --config X` threads --config into every step it spawns,
	// so a backfill that does not accept it aborts the step with "flag provided
	// but not defined" the moment anyone points maintenance at a non-default
	// config.
	configPath := fs.String("config", "", "path to config file")
	batch := fs.Int("batch", 200, "embeddings per batch")
	pause := fs.Duration("pause", 0, "sleep between batches (e.g. 500ms) to stay gentle on CPU")
	max := fs.Int("max", 0, "max memories to embed this run (0 = unlimited)")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	if err := config.EnsureDirs(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "ensure dirs: %v\n", err)
		os.Exit(1)
	}

	memSvc, err := memory.NewService(cfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init memory service: %v\n", err)
		os.Exit(1)
	}
	defer memSvc.Close()

	ctx := context.Background()

	// Normalized hashes first, and before the embeddings guard: the stamp needs
	// no model, and until a row carries one it is invisible to near-duplicate
	// detection on the save path. Bounded per run like the embedding drain.
	if pending, err := memSvc.PendingNormalizedHash(ctx); err == nil && pending > 0 {
		fmt.Printf("anchored: stamping normalized hashes for %d memories…\n", pending)
		stamped, err := memSvc.BackfillNormalizedHash(ctx, normalizedHashBatch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "normalized hash backfill error after %d stamped: %v\n", stamped, err)
		} else if remaining := pending - stamped; remaining > 0 {
			fmt.Printf("anchored: %d stamped, %d left — run again to continue\n", stamped, remaining)
		} else {
			fmt.Printf("anchored: %d normalized hashes stamped\n", stamped)
		}
	}

	if !memSvc.EmbeddingsEnabled() {
		fmt.Fprintln(os.Stderr, "embeddings unavailable (provider 'none' or ONNX model missing) — nothing more to backfill")
		return
	}

	start := time.Now()
	fmt.Println("anchored: backfilling embeddings for memories missing a vector…")

	n, err := memSvc.BackfillEmbeddingsLimited(ctx, *batch, *pause, *max)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill error after %d embedded: %v\n", n, err)
		os.Exit(1)
	}

	fmt.Printf("anchored: backfill complete — %d memories embedded in %s\n", n, time.Since(start).Round(time.Second))

	// When capped, report how much backlog is left so a timer-driven caller
	// (or the user watching a manual run) knows the drain isn't finished.
	if *max > 0 && n >= *max {
		if remaining, err := memSvc.PendingEmbeddings(ctx); err == nil && remaining > 0 {
			fmt.Printf("anchored: %d memories still missing a vector — run again to continue\n", remaining)
		}
	}
}
