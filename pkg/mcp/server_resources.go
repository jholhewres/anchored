package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jholhewres/anchored/pkg/memory"
)

// resourceTexts renders each resource's text, by URI. The SDK answers a URI
// that is not here itself.
func (s *Server) resourceTexts() map[string]func(context.Context) (string, error) {
	return map[string]func(context.Context) (string, error){
		"anchored://memory/stats":  s.statsResource,
		"anchored://memory/recent": s.recentResource,
		"anchored://identity":      identityResource,
		"anchored://projects":      s.projectsResource,
	}
}

func (s *Server) statsResource(ctx context.Context) (string, error) {
	stats, err := s.mem.Stats(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Total: %d\nCategories: %v\nProjects: %v",
		stats.TotalMemories, stats.ByCategory, stats.ByProject), nil
}

func (s *Server) recentResource(ctx context.Context) (string, error) {
	memories, err := s.mem.List(ctx, memory.ListOptions{Limit: 10})
	if err != nil {
		return "", err
	}
	if len(memories) == 0 {
		return "No memories yet.", nil
	}
	var lines []string
	for _, m := range memories {
		lines = append(lines, fmt.Sprintf("[%s] %s", m.Category, m.Content))
	}
	return joinLines(lines), nil
}

func identityResource(context.Context) (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(homeDir, ".anchored", "identity.md"))
	if err != nil {
		return "No identity file configured. Use 'anchored identity edit' to create one.", nil
	}
	return string(data), nil
}

func (s *Server) projectsResource(ctx context.Context) (string, error) {
	rows, err := s.mem.StoreDB().QueryContext(ctx, "SELECT id, name, path FROM projects ORDER BY name")
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
		return "No projects registered.", nil
	}
	return "ID\tName\tPath\n" + joinLines(lines), nil
}
