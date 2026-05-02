package ctx

import (
	"encoding/json"
	"testing"
	"time"
)

func TestChunkJSONRoundTrip(t *testing.T) {
	c := Chunk{
		ID:          "chunk-1",
		SessionID:   "sess-1",
		Source:      "execute",
		Label:       "test output",
		Content:     "hello world",
		ContentType: "code",
		IndexedAt:   time.Now().Truncate(time.Millisecond),
		TTLHours:    336,
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var got Chunk
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	if got.ID != c.ID || got.Source != c.Source || got.Content != c.Content {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, c)
	}
}

func TestExecuteResultFields(t *testing.T) {
	r := ExecuteResult{
		Stdout:    "out",
		Stderr:    "err",
		ExitCode:  0,
		Duration:  2 * time.Second,
		TimedOut:  false,
		Truncated: true,
	}
	if r.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", r.ExitCode)
	}
	if !r.Truncated {
		t.Error("Truncated: got false, want true")
	}
}

func TestBatchCommandSlice(t *testing.T) {
	cmds := []BatchCommand{
		{Label: "build", Command: "go build ./...", Language: "shell"},
		{Label: "test", Command: "go test ./...", Language: "shell"},
	}
	if len(cmds) != 2 {
		t.Fatalf("len: got %d, want 2", len(cmds))
	}
	if cmds[0].Label != "build" {
		t.Errorf("Label: got %q, want %q", cmds[0].Label, "build")
	}
}

func TestFetchResultFromCache(t *testing.T) {
	r := FetchResult{
		Markdown:    "# Title",
		ContentType: "text/html",
		URL:         "https://example.com",
		FetchedAt:   time.Now(),
		FromCache:   true,
	}
	if !r.FromCache {
		t.Error("FromCache: got false, want true")
	}
}

func TestSessionEventPriority(t *testing.T) {
	e := SessionEvent{
		ID:        "evt-1",
		SessionID: "sess-1",
		EventType: "tool_use",
		Priority:  1,
		ToolName:  "bash",
	}
	if e.Priority != 1 {
		t.Errorf("Priority: got %d, want 1", e.Priority)
	}
}

func TestSearchOptsDefaults(t *testing.T) {
	opts := SearchOpts{MaxResults: 10}
	if opts.MaxResults != 10 {
		t.Errorf("MaxResults: got %d, want 10", opts.MaxResults)
	}
	if opts.ContentType != "" {
		t.Errorf("ContentType: got %q, want empty", opts.ContentType)
	}
}

func TestContentSearchResultScore(t *testing.T) {
	r := ContentSearchResult{
		ChunkID: "c-1",
		Label:   "output",
		Source:  "execute",
		Snippet: "matched text",
		Score:   0.95,
	}
	if r.Score <= 0 || r.Score > 1.0 {
		t.Errorf("Score out of range: %f", r.Score)
	}
}

func TestBatchResultJSONRoundTrip(t *testing.T) {
	br := BatchResult{
		Results: []ExecuteResult{
			{Stdout: "hello", ExitCode: 0, Duration: time.Second},
		},
		SourceID:   "batch-1",
		TotalBytes: 1024,
	}
	data, err := json.Marshal(br)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got BatchResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SourceID != br.SourceID || got.TotalBytes != br.TotalBytes {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
	if len(got.Results) != 1 || got.Results[0].Stdout != "hello" {
		t.Errorf("results mismatch: %+v", got.Results)
	}
}
