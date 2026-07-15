package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runBootstrap(args []string) {
	fs := newFlagSet("bootstrap")
	cwd := fs.String("cwd", "", "current working directory")
	configPath := fs.String("config", "", "path to config file")
	dryRun := fs.Bool("dry-run", false, "preview without saving")
	force := fs.Bool("force", false, "overwrite existing memories")
	skipEmbeddings := fs.Bool("skip-embeddings", false, "skip embedding backfill after bootstrap")
	sources := fs.String("source", "readme,docs,rules,tree", "comma-separated sources")
	fs.Parse(args)

	rootDir := *cwd
	if rootDir == "" {
		rootDir = "."
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	sourceList := strings.Split(*sources, ",")
	var seeds []memorySeed

	for _, src := range sourceList {
		switch strings.TrimSpace(src) {
		case "readme":
			seeds = append(seeds, extractReadme(rootDir)...)
		case "docs":
			seeds = append(seeds, extractDocs(rootDir)...)
		case "rules":
			seeds = append(seeds, extractRules(rootDir)...)
		case "tree":
			seeds = append(seeds, extractTree(rootDir)...)
		case "git":
		}
	}

	if len(seeds) == 0 {
		fmt.Println("No bootstrap sources found.")
		return
	}

	saved := 0
	skipped := 0
	ctx := context.Background()

	projectID := svc.ResolveProject(rootDir)

	for _, seed := range seeds {
		if !*force {
			hash := contentHash(seed.Content)
			var count int
			if projectID != "" {
				err := svc.StoreDB().QueryRowContext(ctx,
					"SELECT COUNT(*) FROM memories WHERE content_hash = ? AND project_id = ? AND deleted_at IS NULL",
					hash, projectID,
				).Scan(&count)
				if err == nil && count > 0 {
					skipped++
					continue
				}
			} else {
				err := svc.StoreDB().QueryRowContext(ctx,
					"SELECT COUNT(*) FROM memories WHERE content_hash = ? AND (project_id IS NULL OR project_id = '') AND deleted_at IS NULL",
					hash,
				).Scan(&count)
				if err == nil && count > 0 {
					skipped++
					continue
				}
			}
		}

		if *dryRun {
			fmt.Printf("[dry-run] %s: %s (%d bytes)\n", seed.Category, truncate(seed.Content, 80), len(seed.Content))
			saved++
			continue
		}

		meta := memory.BootstrapMetadata(seed.Confidence)
		_, err := svc.SaveWithOptions(ctx, memory.SaveOptions{
			Content:   seed.Content,
			Category:  seed.Category,
			Source:    "bootstrap",
			CWD:       rootDir,
			Metadata:  meta.ToAny(),
			SkipEmbed: true,
		})
		if err != nil {
			slog.Warn("bootstrap save failed", "error", err, "category", seed.Category)
			continue
		}
		saved++
	}

	action := "saved"
	if *dryRun {
		action = "would save"
	}
	fmt.Printf("Bootstrap: %d memories %s, %d skipped (dedup)\n", saved, action, skipped)

	if !*dryRun && !*skipEmbeddings && svc.EmbeddingsEnabled() {
		var pending int
		svc.StoreDB().QueryRowContext(ctx,
			"SELECT COUNT(*) FROM memories WHERE embedding IS NULL OR LENGTH(embedding) = 0",
		).Scan(&pending)
		if pending > 0 {
			fmt.Fprintf(os.Stderr, "Backfilling %d embeddings (this may take a while)...\n", pending)
			embedded, err := svc.BackfillEmbeddings(ctx, 50)
			if err != nil {
				slog.Warn("embedding backfill failed", "error", err)
			} else {
				fmt.Printf("Embeddings backfilled: %d / %d\n", embedded, pending)
			}
		}
	}
}

type memorySeed struct {
	Category   string
	Content    string
	Confidence float64
}

func extractReadme(root string) []memorySeed {
	path := filepath.Join(root, "README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)
	if len(text) < 50 {
		return nil
	}

	return []memorySeed{{
		Category:   "summary",
		Content:    text,
		Confidence: 0.9,
	}}
}

func extractDocs(root string) []memorySeed {
	docsDir := filepath.Join(root, "docs")
	entries, err := os.ReadDir(docsDir)
	if err != nil {
		return nil
	}

	var seeds []memorySeed
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(docsDir, entry.Name()))
		if err != nil || len(data) < 50 {
			continue
		}
		seeds = append(seeds, memorySeed{
			Category:   "summary",
			Content:    string(data),
			Confidence: 0.7,
		})
	}
	return seeds
}

func extractRules(root string) []memorySeed {
	var seeds []memorySeed
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", "GEMINI.md"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || len(data) < 20 {
			continue
		}
		seeds = append(seeds, memorySeed{
			Category:   "decision",
			Content:    string(data),
			Confidence: 0.95,
		})
	}
	return seeds
}

func extractTree(root string) []memorySeed {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var sb strings.Builder
	sb.WriteString("Project structure:\n")
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
			continue
		}
		if e.IsDir() {
			sb.WriteString("  " + name + "/\n")
		} else {
			sb.WriteString("  " + name + "\n")
		}
	}

	return []memorySeed{{
		Category:   "fact",
		Content:    sb.String(),
		Confidence: 0.85,
	}}
}

func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

