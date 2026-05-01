package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CursorImporter imports Cursor rules from ~/.cursor/rules/*.mdc and ~/.cursor/mcp.json.
type CursorImporter struct {
	baseDir string
	log     func(string, ...any)
}

// NewCursorImporter creates a CursorImporter scanning baseDir (expected: ~/.cursor).
func NewCursorImporter(baseDir string, log func(string, ...any)) *CursorImporter {
	return &CursorImporter{baseDir: baseDir, log: log}
}

func (i *CursorImporter) Name() string { return "cursor" }
func (i *CursorImporter) Path() string { return i.baseDir }

func (i *CursorImporter) Detect() bool {
	rulesDir := filepath.Join(i.baseDir, "rules")
	entries, err := os.ReadDir(rulesDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".mdc") {
				return true
			}
		}
	}

	mcpPath := filepath.Join(i.baseDir, "mcp.json")
	if _, err := os.Stat(mcpPath); err == nil {
		return true
	}
	return false
}

func (i *CursorImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	result := ImportResult{Source: i.Name()}

	result.Found += i.importMDCFiles(ctx, store, &result)
	result.Found += i.importMCPJSON(ctx, store, &result)

	return result
}

func (i *CursorImporter) importMDCFiles(ctx context.Context, store ImportStore, result *ImportResult) int {
	rulesDir := filepath.Join(i.baseDir, "rules")
	entries, err := os.ReadDir(rulesDir)
	if err != nil {
		return 0
	}

	var found int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".mdc") {
			continue
		}

		found++
		filePath := filepath.Join(rulesDir, e.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			result.Errors++
			if i.log != nil {
				i.log("failed to read mdc file", "path", filePath, "error", err)
			}
			continue
		}

		fm, body, err := parseFrontmatter(data)
		if err != nil {
			result.Skipped++
			if i.log != nil {
				i.log("malformed frontmatter, skipping", "path", filePath, "error", err)
			}
			continue
		}

		description := ""
		if v, ok := fm["description"]; ok {
			description, _ = v.(string)
		}

		name := strings.TrimSuffix(e.Name(), ".mdc")
		var content strings.Builder
		content.WriteString("# ")
		content.WriteString(name)
		if description != "" {
			content.WriteString("\n\n")
			content.WriteString(description)
		}
		if body != "" {
			content.WriteString("\n\n")
			content.WriteString(body)
		}

		var globs interface{}
		if v, ok := fm["globs"]; ok {
			globs = v
		}

		metadata := make(map[string]interface{})
		if globs != nil {
			metadata["globs"] = globs
		}
		metadata["file"] = e.Name()

		metaJSON, _ := json.Marshal(metadata)

		// Embed metadata as JSON comment — StoreRaw has no metadata field
		finalContent := content.String() + "\n\n<!-- metadata: " + string(metaJSON) + " -->"

		if err := store.SaveRaw(ctx, finalContent, "preference", "cursor", ""); err != nil {
			result.Errors++
			if i.log != nil {
				i.log("failed to save mdc rule", "path", filePath, "error", err)
			}
			continue
		}

		result.Imported++
	}

	return found
}

func (i *CursorImporter) importMCPJSON(ctx context.Context, store ImportStore, result *ImportResult) int {
	mcpPath := filepath.Join(i.baseDir, "mcp.json")
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		return 0
	}

	content := "# Cursor MCP Configuration\n\n" + string(data)

	if err := store.SaveRaw(ctx, content, "fact", "cursor", ""); err != nil {
		result.Errors++
		if i.log != nil {
			i.log("failed to save mcp.json", "error", err)
		}
		return 1
	}

	result.Imported++
	return 1
}

// parseFrontmatter extracts YAML frontmatter (between --- markers) using
// simple line-based parsing — no YAML dependency required.
func parseFrontmatter(data []byte) (map[string]interface{}, string, error) {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return nil, content, nil // no frontmatter
	}

	rest := content[3:]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		return nil, "", fmt.Errorf("malformed frontmatter: no closing --- found")
	}

	frontmatter := rest[:end]
	body := strings.TrimSpace(rest[end+4:])

	result := make(map[string]interface{})
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if value == "" {
				continue
			}
			result[key] = parseFrontmatterValue(value)
		}
	}

	return result, body, nil
}

func parseFrontmatterValue(value string) interface{} {
	if strings.HasPrefix(value, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(value), &arr); err == nil {
			return arr
		}
	}
	return value
}
