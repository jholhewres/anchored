package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jholhewres/anchored/pkg/memory"
)

// resourceText is the text of the resource at uri.
func (s *Server) resourceText(ctx context.Context, uri string) (string, error) {
	var content string
	switch uri {
	case "anchored://memory/stats":
		stats, err := s.mem.Stats(ctx)
		if err != nil {
			return "", err
		}
		content = fmt.Sprintf("Total: %d\nCategories: %v\nProjects: %v",
			stats.TotalMemories, stats.ByCategory, stats.ByProject)
	case "anchored://memory/recent":
		memories, err := s.mem.List(ctx, memory.ListOptions{Limit: 10})
		if err != nil {
			return "", err
		}
		if len(memories) == 0 {
			content = "No memories yet."
		} else {
			var lines []string
			for _, m := range memories {
				lines = append(lines, fmt.Sprintf("[%s] %s", m.Category, m.Content))
			}
			content = joinLines(lines)
		}
	case "anchored://identity":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		identityPath := filepath.Join(homeDir, ".anchored", "identity.md")
		data, err := os.ReadFile(identityPath)
		if err != nil {
			content = "No identity file configured. Use 'anchored identity edit' to create one."
		} else {
			content = string(data)
		}
	case "anchored://projects":
		db := s.mem.StoreDB()
		rows, err := db.QueryContext(ctx, "SELECT id, name, path FROM projects ORDER BY name")
		if err != nil {
			return "", err
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var pid, name, ppath string
			if err := rows.Scan(&pid, &name, &ppath); err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s", pid, name, ppath))
		}
		if err := rows.Err(); err != nil {
			return "", err
		}
		if len(lines) == 0 {
			content = "No projects registered."
		} else {
			content = "ID\tName\tPath\n" + joinLines(lines)
		}
	default:
		return "", fmt.Errorf("unknown resource: %s", uri)
	}
	return content, nil
}
