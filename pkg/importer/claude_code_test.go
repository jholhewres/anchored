package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type ccMockStore struct {
	mu    sync.Mutex
	saved []ccSaveCall
}

type ccSaveCall struct {
	content  string
	category string
	source   string
	cwd      string
}

func (m *ccMockStore) SaveRaw(_ context.Context, content, category, source, cwd string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, ccSaveCall{content, category, source, cwd})
	return nil
}

func (m *ccMockStore) SaveRawWithSource(_ context.Context, content, category, source string, _ *string, cwd string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, ccSaveCall{content, category, source, cwd})
	return nil
}

func TestExtractText_StringContent(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	text := extractText(raw)
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
}

func TestExtractText_ArrayContent(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"hello"},{"type":"tool_use","name":"Read","input":{}},{"type":"text","text":"world"}]`)
	text := extractText(raw)
	if text != "hello\nworld" {
		t.Errorf("expected 'hello\\nworld', got %q", text)
	}
}

func TestExtractText_Empty(t *testing.T) {
	if extractText(nil) != "" {
		t.Error("expected empty for nil")
	}
	if extractText(json.RawMessage(`""`)) != "" {
		t.Error("expected empty for empty string")
	}
}

func TestExtractTexts(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"first"},{"type":"text","text":"second"}]`)
	texts := extractTexts(raw)
	if len(texts) != 2 {
		t.Fatalf("expected 2 texts, got %d", len(texts))
	}
	if texts[0] != "first" || texts[1] != "second" {
		t.Errorf("unexpected texts: %v", texts)
	}
}

func TestExtractTexts_StringInput(t *testing.T) {
	raw := json.RawMessage(`"single text"`)
	texts := extractTexts(raw)
	if len(texts) != 1 || texts[0] != "single text" {
		t.Errorf("expected ['single text'], got %v", texts)
	}
}

func TestExtractToolCalls(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"some text"},
		{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/test.go"}},
		{"type":"tool_use","name":"Bash","input":{"command":"ls"}}
	]`)
	calls := extractToolCalls(raw)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].Name != "Read" {
		t.Errorf("expected Read, got %s", calls[0].Name)
	}
	if calls[1].Name != "Bash" {
		t.Errorf("expected Bash, got %s", calls[1].Name)
	}
}

func TestExtractToolCalls_Empty(t *testing.T) {
	calls := extractToolCalls(json.RawMessage(`"just a string"`))
	if len(calls) != 0 {
		t.Errorf("expected 0 tool calls from string content, got %d", len(calls))
	}
}

func TestContentHash_Deduplication(t *testing.T) {
	h1 := contentHash("same content")
	h2 := contentHash("same content")
	h3 := contentHash("different content")
	if h1 != h2 {
		t.Error("same content should produce same hash")
	}
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}
}

func TestDirToPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"-home-jhol-Workspace-private-anchored", "/home/jhol/Workspace/private/anchored"},
		{"-home-user-project", "/home/user/project"},
		{"no-dash-prefix", ""},
		{"-", ""},
	}
	for _, tt := range tests {
		got := dirToPath(tt.input)
		if got != tt.want {
			t.Errorf("dirToPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseFrontmatterCategory(t *testing.T) {
	tests := []struct {
		fm   string
		want string
	}{
		{"type: project\n", "preference"},
		{"type: global\n", "preference"},
		{"type: summary\n", "summary"},
		{"category: decision\n", "decision"},
		{"type: unknown\n", ""},
		{"no relevant keys\n", ""},
	}
	for _, tt := range tests {
		got := parseFrontmatterCategory(tt.fm)
		if got != tt.want {
			t.Errorf("parseFrontmatterCategory(%q) = %q, want %q", tt.fm, got, tt.want)
		}
	}
}

func TestImportJSONL_UserStringContent(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"user","message":{"role":"user","content":"I prefer dark mode"},"cwd":"/test/project","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 1 {
		t.Fatalf("expected 1 imported, got %d", count)
	}
	if len(store.saved) != 1 {
		t.Fatalf("expected 1 saved, got %d", len(store.saved))
	}
	if store.saved[0].content != "I prefer dark mode" {
		t.Errorf("unexpected content: %q", store.saved[0].content)
	}
	if store.saved[0].cwd != "/test/project" {
		t.Errorf("unexpected cwd: %q", store.saved[0].cwd)
	}
}

func TestImportJSONL_AssistantArrayContent(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The architecture uses microservices"},{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/main.go"}}]},"cwd":"/test/project","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 1 {
		t.Fatalf("expected 1 imported text entry, got %d", count)
	}

	found := map[string]bool{}
	for _, s := range store.saved {
		found[s.category] = true
	}
	if !found["decision"] {
		t.Error("expected 'decision' category for architecture text")
	}
}

func TestImportJSONL_SummaryEntry(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"summary","message":{"role":"assistant","content":"Compacted summary of the session work"},"cwd":"/test/project","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 1 {
		t.Fatalf("expected 1 summary imported, got %d", count)
	}
	if store.saved[0].category != "summary" {
		t.Errorf("expected category 'summary', got %q", store.saved[0].category)
	}
}

func TestImportJSONL_AttachmentEntry(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := fmt.Sprintf(`{"type":"attachment","attachment":{"type":"hook_success","hookName":"SessionStart:startup","hookEvent":"SessionStart","content":"session initialized","stdout":""},"cwd":"/test/project","sessionId":"s1"}
`)
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 0 {
		t.Fatalf("expected 0 attachment imports, got %d", count)
	}
	if len(store.saved) != 0 {
		t.Fatalf("expected no saved attachment memories, got %d", len(store.saved))
	}
}

func TestImportJSONL_Deduplication(t *testing.T) {
	tmpDir := t.TempDir()
	line := `{"type":"user","message":{"role":"user","content":"duplicate message"},"cwd":"/test","sessionId":"s1"}` + "\n"
	jsonlContent := line + line + line
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 3 {
		t.Fatalf("expected 3 saved (dedup is DB-level), got %d", count)
	}
}

func TestImportJSONL_CorruptLine(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{corrupt json}
{"type":"user","message":{"role":"user","content":"valid content"},"cwd":"/test","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 1 {
		t.Fatalf("expected 1 imported (corrupt skipped), got %d", count)
	}
}

func TestImportJSONL_IgnoresToolResult(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"abc","content":"file content"}]},"cwd":"/test","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 0 {
		t.Fatalf("expected 0 imported (tool_result ignored), got %d", count)
	}
}

func TestImportJSONL_IgnoresThinking(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"internal thoughts"}]},"cwd":"/test","sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importJSONL(context.Background(), store, jsonlPath, "s1", false)

	if count != 0 {
		t.Fatalf("expected 0 imported (thinking ignored), got %d", count)
	}
}

func TestImportJSONL_CWDFallback(t *testing.T) {
	tmpDir := t.TempDir()
	jsonlContent := `{"type":"user","message":{"role":"user","content":"hello"},"sessionId":"s1"}
`
	jsonlPath := filepath.Join(tmpDir, "test.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(jsonlContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	imp.importJSONL(context.Background(), store, jsonlPath, "-home-user-myproject", false)

	if len(store.saved) > 0 && store.saved[0].cwd != "/home/user/myproject" {
		t.Errorf("expected cwd fallback to '/home/user/myproject', got %q", store.saved[0].cwd)
	}
}

func TestImportMemoryFiles(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "-home-test-project")
	memDir := filepath.Join(projectDir, "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}

	content := "---\ntype: project\n---\nUse TypeScript strict mode always."
	if err := os.WriteFile(filepath.Join(memDir, "rules.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importMemoryFiles(context.Background(), store)

	if count != 1 {
		t.Fatalf("expected 1 memory file imported, got %d", count)
	}
	if store.saved[0].category != "preference" {
		t.Errorf("expected 'preference' from frontmatter, got %q", store.saved[0].category)
	}
}

func TestImportGlobalCLAUDE(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, "projects")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "CLAUDE.md"), []byte("# Global rules\nAlways use Go."), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: claudeDir, log: func(string, ...any) {}}
	count := imp.importGlobalCLAUDE(context.Background(), store)

	if count != 1 {
		t.Fatalf("expected 1 CLAUDE.md imported, got %d", count)
	}
	if store.saved[0].category != "preference" {
		t.Errorf("expected 'preference', got %q", store.saved[0].category)
	}
}

func TestImportSession_Subagents(t *testing.T) {
	tmpDir := t.TempDir()
	sessionDir := filepath.Join(tmpDir, "-home-test-project")
	subagentDir := filepath.Join(sessionDir, "subagents")
	if err := os.MkdirAll(subagentDir, 0755); err != nil {
		t.Fatal(err)
	}

	mainContent := `{"type":"user","message":{"role":"user","content":"main message"},"cwd":"/test","sessionId":"s1"}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "main.jsonl"), []byte(mainContent), 0644); err != nil {
		t.Fatal(err)
	}

	subContent := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"subagent result"}]},"cwd":"/test","sessionId":"s1"}` + "\n"
	if err := os.WriteFile(filepath.Join(subagentDir, "agent-abc.jsonl"), []byte(subContent), 0644); err != nil {
		t.Fatal(err)
	}

	store := &ccMockStore{}
	imp := &ClaudeCodeImporter{baseDir: tmpDir, log: func(string, ...any) {}}
	count := imp.importSession(context.Background(), store, sessionDir, "-home-test-project", false)

	if count != 2 {
		t.Fatalf("expected 2 imported (1 main + 1 subagent), got %d", count)
	}
}
