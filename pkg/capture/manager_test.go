package capture

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

type mockSaveStore struct {
	mu       sync.Mutex
	memories map[string]*memory.Memory
}

func newMockSaveStore() *mockSaveStore {
	return &mockSaveStore{memories: make(map[string]*memory.Memory)}
}

func (m *mockSaveStore) SaveWithOptions(ctx context.Context, opts memory.SaveOptions) (*memory.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	id := "test-id-" + opts.Category
	mem := &memory.Memory{
		ID:        id,
		Content:   opts.Content,
		Category:  opts.Category,
		Source:    opts.Source,
		CreatedAt: time.Now(),
	}
	m.memories[id] = mem
	return mem, nil
}

func (m *mockSaveStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.memories)
}

func (m *mockSaveStore) lastContent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mem := range m.memories {
		return mem.Content
	}
	return ""
}

func TestCaptureSession_HighQuality(t *testing.T) {
	store := newMockSaveStore()
	extractor := NewSummaryExtractor()
	mgr := NewAutoCaptureManager(store, extractor, nil, nil)

	toolCalls := []ToolCall{
		{
			Tool:      "edit",
			Input:     "We decided to use SQLite for storage because it requires no external dependencies.",
			Output:    "The project uses SQLite as its primary database engine.",
			Timestamp: time.Now(),
		},
		{
			Tool:      "bash",
			Input:     "go test ./...",
			Output:    "ok  passed all tests",
			Timestamp: time.Now(),
		},
	}

	err := mgr.CaptureSession(context.Background(), "sess-high", toolCalls, "/tmp/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.count() != 1 {
		t.Fatalf("expected 1 memory saved, got %d", store.count())
	}

	content := store.lastContent()
	if !strings.Contains(content, "Decisions") && !strings.Contains(content, "Key Facts") {
		t.Errorf("expected summary to contain Decisions or Key Facts, got:\n%s", content)
	}
}

func TestCaptureSession_LowQuality(t *testing.T) {
	store := newMockSaveStore()
	extractor := NewSummaryExtractor()
	mgr := NewAutoCaptureManager(store, extractor, nil, nil)

	toolCalls := []ToolCall{
		{
			Tool:      "bash",
			Input:     "ls",
			Output:    "file1.txt file2.txt",
			Timestamp: time.Now(),
		},
	}

	err := mgr.CaptureSession(context.Background(), "sess-low", toolCalls, "/tmp/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.count() != 0 {
		t.Fatalf("expected 0 memories (low quality), got %d", store.count())
	}
}

func TestCaptureSession_TooShort(t *testing.T) {
	store := newMockSaveStore()
	extractor := NewSummaryExtractor()
	mgr := NewAutoCaptureManager(store, extractor, nil, nil)

	toolCalls := []ToolCall{
		{
			Tool:      "bash",
			Input:     "ok",
			Output:    "done",
			Timestamp: time.Now(),
		},
	}

	err := mgr.CaptureSession(context.Background(), "sess-short", toolCalls, "/tmp/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.count() != 0 {
		t.Fatalf("expected 0 memories (too short), got %d", store.count())
	}
}

func TestCaptureSession_EmptySession(t *testing.T) {
	store := newMockSaveStore()
	extractor := NewSummaryExtractor()
	mgr := NewAutoCaptureManager(store, extractor, nil, nil)

	err := mgr.CaptureSession(context.Background(), "sess-empty", nil, "/tmp/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.count() != 0 {
		t.Fatalf("expected 0 memories (empty session), got %d", store.count())
	}
}

func TestCaptureSession_SecretsRedacted(t *testing.T) {
	store := newMockSaveStore()
	extractor := NewSummaryExtractor()
	sanitizer := memory.NewSanitizer(true)
	mgr := NewAutoCaptureManager(store, extractor, sanitizer, nil)

	toolCalls := []ToolCall{
		{
			Tool: "edit",
			Input: `We decided to use the external API for authentication.
The config contains API_KEY=sk-abc123def456ghi789jkl012mno345pqr678.
This approach uses OAuth tokens for security.`,
			Output:    "Configuration updated with API_KEY and token settings.",
			Timestamp: time.Now(),
		},
	}

	err := mgr.CaptureSession(context.Background(), "sess-secrets", toolCalls, "/tmp/project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.count() == 0 {
		t.Fatal("expected 1 memory saved, got 0")
	}

	content := store.lastContent()
	if strings.Contains(content, "sk-abc123") {
		t.Errorf("secret was not redacted from saved content:\n%s", content)
	}
}
