package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runSearch(args []string) {
	fs := newFlagSet("search")
	category := fs.String("category", "", "filter by category")
	project := fs.String("project", "", "filter by project ID")
	cwd := fs.String("cwd", "", "current working directory for project detection")
	global := fs.Bool("global", false, "search across all projects")
	limit := fs.Int("limit", 10, "max results")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	query := strings.Join(fs.Args(), " ")
	if query == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored search <query> [--category] [--project] [--cwd] [--global] [--limit]")
		os.Exit(1)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()
	projectID := *project
	if projectID == "" && *cwd != "" {
		projectID = svc.ResolveProject(*cwd)
	}
	if projectID != "" {
		resolved, err := resolveProjectFilter(ctx, svc, projectID)
		if err != nil {
			// project not found — search without project filter
			projectID = ""
		} else {
			projectID = resolved
		}
	}

	if *global {
		projectID = ""
	}

	opts := memory.SearchOptions{
		MaxResults: *limit,
		Category:   *category,
		ProjectID:  projectID,
	}

	results, err := svc.Search(ctx, query, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "search error: %v\n", err)
		os.Exit(1)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return
	}

	for i, r := range results {
		projectLabel := ""
		if *global && r.Memory.ProjectID != nil && *r.Memory.ProjectID != "" {
			projectLabel = fmt.Sprintf(" project=%s", *r.Memory.ProjectID)
		}
		fmt.Printf("%d. [%s]%s %s (score=%.3f id=%s)\n", i+1, r.Memory.Category, projectLabel, truncate(r.Memory.Content, 120), r.Score, r.Memory.ID)
	}
}

func resolveProjectFilter(ctx context.Context, svc *memory.Service, spec string) (string, error) {
	var id string
	err := svc.StoreDB().QueryRowContext(ctx,
		`SELECT id FROM projects
		 WHERE id = ? OR name = ? OR path = ? OR path LIKE ?
		 ORDER BY CASE WHEN id = ? THEN 0 WHEN name = ? THEN 1 WHEN path = ? THEN 2 ELSE 3 END
		 LIMIT 1`,
		spec, spec, spec, "%/"+spec, spec, spec, spec,
	).Scan(&id)
	if err == nil {
		return id, nil
	}

	rows, err := svc.StoreDB().QueryContext(ctx, "SELECT id, name, path FROM projects")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	normalizedSpec := normalizeProjectSpec(spec)
	for rows.Next() {
		var projectID, name, path string
		if err := rows.Scan(&projectID, &name, &path); err != nil {
			return "", err
		}
		if normalizeProjectSpec(name) == normalizedSpec || normalizeProjectSpec(filepath.Base(path)) == normalizedSpec {
			return projectID, nil
		}
	}
	return "", fmt.Errorf("project %q not found", spec)
}

func normalizeProjectSpec(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	replacer := strings.NewReplacer(".", "", "-", "", "_", "", " ", "")
	return replacer.Replace(s)
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
