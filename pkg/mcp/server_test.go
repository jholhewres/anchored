package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetDebugLogger(t *testing.T) {
	s := NewServer(nil, nil, nil, nil, nil, "", nil)
	
	// Test with nil logger (should not panic)
	s.SetDebugLogger(nil)
	if s.dlog != nil {
		t.Error("expected dlog to be nil after SetDebugLogger(nil)")
	}
}

func TestHeadTail(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		head     int
		tail     int
		expected string
	}{
		{
			name:     "short string passes through",
			s:        "hello",
			head:     10,
			tail:     10,
			expected: "hello",
		},
		{
			name:     "truncates with head and tail",
			s:        "abcdefghijklmnopqrstuvwxyz",
			head:     5,
			tail:     5,
			expected: "abcde\n[... 16 bytes omitted ...]\nvwxyz",
		},
		{
			name:     "empty string",
			s:        "",
			head:     5,
			tail:     5,
			expected: "",
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := headTail(tt.s, tt.head, tt.tail)
			if result != tt.expected {
				t.Errorf("headTail(%q, %d, %d) = %q; want %q", tt.s, tt.head, tt.tail, result, tt.expected)
			}
		})
	}
}

func TestMarshalResponse_ErrorPath(t *testing.T) {
	// Force json.Marshal to fail by putting a non-marshalable value (channel) in Result.
	// JSONRPCResponse.Result is `any`, so a chan triggers a marshal error, exercising the
	// error branch that wraps the failure into an InternalError response.
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage("1"),
		Result:  make(chan int), // channels cannot be JSON-marshaled
	}
	out := MarshalResponse(resp)
	if len(out) == 0 {
		t.Fatal("expected non-empty fallback response")
	}
	if !strings.Contains(string(out), "error") {
		t.Errorf("expected error payload in fallback, got: %s", out)
	}
}

func TestSearchHitWriter_HitAndOmit(t *testing.T) {
	w := newSearchHitWriter(false)
	w.open("<anchored_search query=\"%s\">", "test")

	// Normal hit: within budget.
	w.hit([]string{`id="1"`}, "first line\nsecond line")
	out := w.sb.String()
	if !strings.Contains(out, "<hit") || !strings.Contains(out, "first line second line") {
		t.Errorf("expected flattened hit content, got: %s", out)
	}

	// Force the omitted path: fill the buffer near the 8KB budget with large hits,
	// then add one more that cannot fit. Content is capped to searchHitRunes (700)
	// per hit, so we need several to exhaust the budget.
	big := strings.Repeat("y", searchHitRunes)
	for i := 0; i < 12; i++ {
		w.hit([]string{`id="fill"`}, big)
	}
	w.hit([]string{`id="overflow"`}, big)
	if w.omitted == 0 {
		t.Error("expected omitted counter to increment when hit exceeds budget")
	}

	// close() must emit the <omitted> note (omitted > 0) and the closing tag.
	closed := w.close()
	if !strings.Contains(closed, "<omitted") {
		t.Errorf("expected <omitted> note in output, got: %s", closed)
	}
	if !strings.Contains(closed, "</anchored_search>") {
		t.Error("expected closing tag in output")
	}
}

func TestSearchHitWriter_CloseWithoutOmit(t *testing.T) {
	w := newSearchHitWriter(true) // full=true branch
	w.open("<anchored_search>")
	closed := w.close()
	if strings.Contains(closed, "<omitted") {
		t.Errorf("did not expect <omitted> note when nothing was omitted, got: %s", closed)
	}
	if !strings.HasSuffix(strings.TrimSpace(closed), "</anchored_search>") {
		t.Errorf("expected output to end with closing tag, got: %s", closed)
	}
}
