package importer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

type ClaudeCodeImporter struct {
	baseDir string
	log     func(string, ...any)
}

func NewClaudeCodeImporter(baseDir string, log func(string, ...any)) *ClaudeCodeImporter {
	return &ClaudeCodeImporter{baseDir: baseDir, log: log}
}

func (i *ClaudeCodeImporter) Name() string { return "claude-code" }
func (i *ClaudeCodeImporter) Path() string { return i.baseDir }

func (i *ClaudeCodeImporter) Detect() bool {
	_, err := os.Stat(i.baseDir)
	return err == nil
}

func (i *ClaudeCodeImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	result := ImportResult{Source: i.Name()}

	result.Imported += i.importMemoryFiles(ctx, store)
	result.Imported += i.importGlobalCLAUDE(ctx, store)

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
		result.Imported += i.importSession(ctx, store, sessionPath, sd.Name(), false)
	}

	return result
}

func (i *ClaudeCodeImporter) importMemoryFiles(ctx context.Context, store ImportStore) int {
	var count int
	entries, err := os.ReadDir(i.baseDir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		memDir := filepath.Join(i.baseDir, e.Name(), "memory")
		files, err := os.ReadDir(memDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			path := filepath.Join(memDir, f.Name())
			count += i.importMemoryFile(ctx, store, path, e.Name())
		}
	}
	return count
}

func (i *ClaudeCodeImporter) importMemoryFile(ctx context.Context, store ImportStore, path, projectDir string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0
	}

	cwd := dirToPath(projectDir)

	body := text
	category := ""
	if strings.HasPrefix(text, "---") {
		end := strings.Index(text[3:], "---")
		if end > 0 {
			fm := text[3 : end+3]
			body = strings.TrimSpace(text[end+6:])
			category = parseFrontmatterCategory(fm)
		}
	}

	if category == "" {
		category = memory.Categorize(body)
	}

	if err := store.SaveRaw(ctx, body, category, "claude-code", cwd); err != nil {
		return 0
	}
	return 1
}

func (i *ClaudeCodeImporter) importGlobalCLAUDE(ctx context.Context, store ImportStore) int {
	path := filepath.Join(i.baseDir, "..", "CLAUDE.md")
	absPath, err := filepath.Abs(path)
	if err != nil {
		return 0
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return 0
	}
	if err := store.SaveRaw(ctx, text, "preference", "claude-code", ""); err != nil {
		return 0
	}
	return 1
}

func (i *ClaudeCodeImporter) importSession(ctx context.Context, store ImportStore, sessionPath, sessionID string, isSubagent bool) int {
	jsonlFiles, err := filepath.Glob(filepath.Join(sessionPath, "*.jsonl"))
	if err != nil || len(jsonlFiles) == 0 {
		return 0
	}

	var count int
	for _, f := range jsonlFiles {
		count += i.importJSONL(ctx, store, f, sessionID, isSubagent)
	}

	subagentDir := filepath.Join(sessionPath, "subagents")
	subFiles, err := os.ReadDir(subagentDir)
	if err != nil {
		return count
	}
	for _, sf := range subFiles {
		if sf.IsDir() {
			count += i.importSession(ctx, store, filepath.Join(subagentDir, sf.Name()), sf.Name(), true)
		} else if strings.HasSuffix(sf.Name(), ".jsonl") {
			count += i.importJSONL(ctx, store, filepath.Join(subagentDir, sf.Name()), sessionID, true)
		}
	}

	return count
}

func (i *ClaudeCodeImporter) importJSONL(ctx context.Context, store ImportStore, path, sessionID string, isSubagent bool) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	var count int

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		cwd := entry.CWD
		if cwd == "" {
			cwd = dirToPath(sessionID)
		}

		switch entry.Type {
		case "user":
			count += i.processUserEntry(ctx, store, &entry, cwd)
		case "assistant":
			count += i.processAssistantEntry(ctx, store, &entry, cwd, isSubagent)
		case "summary":
			count += i.processSummaryEntry(ctx, store, &entry, cwd)
		case "attachment":
			count += i.processAttachmentEntry(ctx, store, json.RawMessage(line), cwd)
		}

		if count > 0 && count%50 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}

	return count
}

func (i *ClaudeCodeImporter) processUserEntry(ctx context.Context, store ImportStore, entry *jsonlEntry, cwd string) int {
	text := extractText(entry.Message.Content)
	if !isUsefulClaudeMemory(text, false) {
		return 0
	}
	return saveImport(ctx, store, text, memory.Categorize(text), "claude-code", cwd, i.log)
}

func (i *ClaudeCodeImporter) processAssistantEntry(ctx context.Context, store ImportStore, entry *jsonlEntry, cwd string, isSubagent bool) int {
	var count int

	for _, text := range extractTexts(entry.Message.Content) {
		if !isUsefulClaudeMemory(text, true) {
			continue
		}
		cat := memory.Categorize(text)
		count += saveImport(ctx, store, text, cat, "claude-code", cwd, i.log)
	}

	return count
}

func (i *ClaudeCodeImporter) processSummaryEntry(ctx context.Context, store ImportStore, entry *jsonlEntry, cwd string) int {
	text := extractText(entry.Message.Content)
	if !isUsefulClaudeMemory(text, false) {
		return 0
	}
	return saveImport(ctx, store, text, "summary", "claude-code", cwd, i.log)
}

func (i *ClaudeCodeImporter) processAttachmentEntry(ctx context.Context, store ImportStore, raw json.RawMessage, cwd string) int {
	return 0
}

func isUsefulClaudeMemory(text string, assistant bool) bool {
	text = strings.TrimSpace(text)
	if len(text) < 12 {
		return false
	}
	lower := strings.ToLower(text)
	if strings.HasPrefix(lower, "tool:") || strings.Contains(lower, "\"tool_use\"") {
		return false
	}
	noisyFragments := []string{"<task-notification", "<tool-use-id>", "hookspecificoutput", "permissiondecision", "pretooluse", "posttooluse"}
	for _, fragment := range noisyFragments {
		if strings.Contains(lower, fragment) {
			return false
		}
	}
	lowValuePrefixes := []string{
		"i'll ", "i’ll ", "let me ", "now ", "great", "got it", "excellent", "i have ", "vou ", "agora ", "perfeito", "ótimo",
	}
	if assistant {
		for _, prefix := range lowValuePrefixes {
			if strings.HasPrefix(lower, prefix) && len(text) < 240 {
				return false
			}
		}
	}
	lowValueExact := []string{"continue", "prossiga", "pode seguir", "ok", "sim", "não"}
	for _, exact := range lowValueExact {
		if lower == exact {
			return false
		}
	}
	return true
}

type jsonlEntry struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	CWD       string `json:"cwd"`
	SessionID string `json:"sessionId"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// extractText handles content as either string or []contentBlock.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				texts = append(texts, strings.TrimSpace(b.Text))
			}
		}
		return strings.Join(texts, "\n")
	}

	return ""
}

func extractTexts(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s != "" {
			return []string{s}
		}
		return nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var texts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				texts = append(texts, strings.TrimSpace(b.Text))
			}
		}
		return texts
	}

	return nil
}

type toolCall struct {
	Name  string
	Input json.RawMessage
}

func extractToolCalls(raw json.RawMessage) []toolCall {
	if len(raw) == 0 {
		return nil
	}

	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}

	var calls []toolCall
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name != "" {
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			calls = append(calls, toolCall{Name: b.Name, Input: input})
		}
	}
	return calls
}

func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func saveImport(ctx context.Context, store ImportStore, content, category, source, cwd string, log func(string, ...any)) int {
	if err := store.SaveRaw(ctx, content, category, source, cwd); err != nil {
		if log != nil {
			log("skip message", "error", err)
		}
		return 0
	}
	return 1
}

// dirToPath converts a Claude Code session dir name to a filesystem path.
// E.g., "-home-jhol-Workspace-private-anchored" → "/home/jhol/Workspace/private/anchored"
func dirToPath(dir string) string {
	name := strings.TrimPrefix(filepath.Base(dir), "-")
	if name == "" || name == dir {
		return ""
	}
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}
	return "/" + strings.Join(parts, "/")
}

func parseFrontmatterCategory(fm string) string {
	lines := strings.Split(fm, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		for _, key := range []string{"type:", "category:"} {
			if strings.HasPrefix(line, key) {
				val := strings.TrimSpace(strings.TrimPrefix(line, key))
				val = strings.Trim(val, "\"'")
				switch val {
				case "project", "global":
					return "preference"
				case "summary", "technical", "decision", "preference", "fact", "event", "learning", "plan":
					return val
				}
			}
		}
	}
	return ""
}
