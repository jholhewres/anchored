package importer

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type ClaudeCodeImporter struct {
	baseDir string
	log     func(string, ...any)
}

func NewClaudeCodeImporter(baseDir string, log func(string, ...any)) *ClaudeCodeImporter {
	return &ClaudeCodeImporter{baseDir: baseDir, log: log}
}

func (i *ClaudeCodeImporter) Name() string { return "claude-code" }

func (i *ClaudeCodeImporter) Detect() bool {
	_, err := os.Stat(i.baseDir)
	return err == nil
}

func (i *ClaudeCodeImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	result := ImportResult{Source: i.Name()}

	sessionDirs, err := os.ReadDir(i.baseDir)
	if err != nil {
		result.Errors++
		return result
	}

	for _, sd := range sessionDirs {
		if !sd.IsDir() {
			continue
		}
		sessionPath := filepath.Join(i.baseDir, sd.Name())
		imported := i.importSession(ctx, store, sessionPath, sd.Name())
		result.Imported += imported
	}

	return result
}

func (i *ClaudeCodeImporter) importSession(ctx context.Context, store ImportStore, sessionPath, sessionID string) int {
	jsonlFiles, err := filepath.Glob(filepath.Join(sessionPath, "*.jsonl"))
	if err != nil || len(jsonlFiles) == 0 {
		return 0
	}

	// skip subagent sessions
	if _, err := os.Stat(filepath.Join(sessionPath, "subagents")); err == nil {
		return 0
	}

	var count int
	for _, f := range jsonlFiles {
		n := i.importJSONL(ctx, store, f, sessionID)
		count += n
	}
	return count
}

func (i *ClaudeCodeImporter) importJSONL(ctx context.Context, store ImportStore, path, sessionID string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	seen := make(map[string]bool)
	var count int

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
			SessionID string `json:"sessionId"`
			CWD       string `json:"cwd"`
		}

		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		if entry.Message.Role != "user" && entry.Message.Role != "assistant" {
			continue
		}

		cwd := entry.CWD
		if cwd == "" && entry.SessionID != "" {
			cwd = extractProjectFromSessionDir(filepath.Join(i.baseDir, entry.SessionID))
		}

		for _, block := range entry.Message.Content {
			if block.Type != "text" || strings.TrimSpace(block.Text) == "" {
				continue
			}

			content := strings.TrimSpace(block.Text)
			dedupKey := content
			if len(dedupKey) > 200 {
			dedupKey = dedupKey[:200]
		}
			if seen[dedupKey] {
				continue
			}
			seen[dedupKey] = true

			category := map[string]string{
				"user":      "decision",
				"assistant": "technical",
			}[entry.Message.Role]

			if err := store.SaveRaw(ctx, content, category, "claude-code", cwd); err != nil {
				if i.log != nil {
					i.log("skip message", "error", err)
				}
				continue
			}
			count++
		}

		// throttle every 50 messages
		if count > 0 && count%50 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	return count
}

func extractProjectFromSessionDir(sessionDir string) string {
	// session dirs look like: -home-jhol-Workspace-private-anchored
	parts := strings.Split(filepath.Base(sessionDir), "-")
	if len(parts) >= 4 {
		// convert -home-jhol-Workspace-private-anchored -> /home/jhol/Workspace/private/anchored
		rel := strings.Join(parts[1:], "/")
		return "/" + rel
	}
	return ""
}


