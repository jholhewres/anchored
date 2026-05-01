package memory

import (
	"encoding/json"
	"testing"
)

func TestMemoryMetadata_MarshalJSON(t *testing.T) {
	m := MemoryMetadata{
		Source:        "auto_capture",
		SessionID:     "sess_123",
		Consolidated:  []string{"id1", "id2"},
		DreamVersion:  "dream_v3",
		CaptureReason: "high relevance",
		QualityScore:  0.95,
	}

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("MarshalJSON error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map error: %v", err)
	}

	if raw["source"] != "auto_capture" {
		t.Errorf("expected source=auto_capture, got %v", raw["source"])
	}
	if raw["session_id"] != "sess_123" {
		t.Errorf("expected session_id=sess_123, got %v", raw["session_id"])
	}
	if raw["dream_version"] != "dream_v3" {
		t.Errorf("expected dream_version=dream_v3, got %v", raw["dream_version"])
	}
	if raw["capture_reason"] != "high relevance" {
		t.Errorf("expected capture_reason='high relevance', got %v", raw["capture_reason"])
	}
	if raw["quality_score"] != 0.95 {
		t.Errorf("expected quality_score=0.95, got %v", raw["quality_score"])
	}

	consolidated, ok := raw["consolidated"].([]any)
	if !ok || len(consolidated) != 2 {
		t.Errorf("expected consolidated to have 2 elements, got %v", raw["consolidated"])
	}
}

func TestMemoryMetadata_UnmarshalJSON(t *testing.T) {
	input := `{"source":"user","session_id":"s1","consolidated":["a","b"],"dream_version":"v1","capture_reason":"test","quality_score":0.5}`

	var m MemoryMetadata
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatalf("UnmarshalJSON error: %v", err)
	}

	if m.Source != "user" {
		t.Errorf("expected Source=user, got %q", m.Source)
	}
	if m.SessionID != "s1" {
		t.Errorf("expected SessionID=s1, got %q", m.SessionID)
	}
	if m.DreamVersion != "v1" {
		t.Errorf("expected DreamVersion=v1, got %q", m.DreamVersion)
	}
	if m.CaptureReason != "test" {
		t.Errorf("expected CaptureReason=test, got %q", m.CaptureReason)
	}
	if m.QualityScore != 0.5 {
		t.Errorf("expected QualityScore=0.5, got %f", m.QualityScore)
	}
	if len(m.Consolidated) != 2 || m.Consolidated[0] != "a" || m.Consolidated[1] != "b" {
		t.Errorf("expected Consolidated=[a b], got %v", m.Consolidated)
	}
}

func TestMemoryMetadata_RoundTrip(t *testing.T) {
	original := MemoryMetadata{
		Source:        "dream",
		SessionID:     "session_42",
		Consolidated:  []string{"mem1", "mem2", "mem3"},
		DreamVersion:  "dream_2026_04_30",
		CaptureReason: "consolidation",
		QualityScore:  0.88,
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded MemoryMetadata
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Source != original.Source || decoded.SessionID != original.SessionID || decoded.DreamVersion != original.DreamVersion || decoded.CaptureReason != original.CaptureReason || decoded.QualityScore != original.QualityScore {
		t.Errorf("round-trip scalar mismatch:\n  original: %+v\n  decoded:  %+v", original, decoded)
	}
	if len(decoded.Consolidated) != len(original.Consolidated) {
		t.Fatalf("consolidated length mismatch: %d vs %d", len(decoded.Consolidated), len(original.Consolidated))
	}
	for i := range decoded.Consolidated {
		if decoded.Consolidated[i] != original.Consolidated[i] {
			t.Errorf("consolidated[%d] mismatch: %q vs %q", i, decoded.Consolidated[i], original.Consolidated[i])
		}
	}
}

func TestMemoryMetadata_ParseMetadata_Nil(t *testing.T) {
	m := ParseMetadata(nil)
	if m.Source != "" || m.SessionID != "" || m.QualityScore != 0 {
		t.Errorf("expected zero value from nil, got %+v", m)
	}
}

func TestMemoryMetadata_ParseMetadata_EmptyMap(t *testing.T) {
	m := ParseMetadata(map[string]any{})
	if m.Source != "" || m.SessionID != "" || m.QualityScore != 0 {
		t.Errorf("expected zero value from empty map, got %+v", m)
	}
}

func TestMemoryMetadata_ParseMetadata_DirectType(t *testing.T) {
	original := MemoryMetadata{Source: "import", SessionID: "s99"}
	m := ParseMetadata(original)
	if m.Source != original.Source || m.SessionID != original.SessionID {
		t.Errorf("expected direct pass-through: got Source=%q SessionID=%q", m.Source, m.SessionID)
	}
}

func TestMemoryMetadata_ParseMetadata_FromMap(t *testing.T) {
	v := map[string]any{
		"source":         "auto_capture",
		"session_id":     "s1",
		"quality_score":  0.75,
		"consolidated":   []any{"c1", "c2"},
	}
	m := ParseMetadata(v)
	if m.Source != "auto_capture" {
		t.Errorf("expected Source=auto_capture, got %q", m.Source)
	}
	if m.SessionID != "s1" {
		t.Errorf("expected SessionID=s1, got %q", m.SessionID)
	}
	if m.QualityScore != 0.75 {
		t.Errorf("expected QualityScore=0.75, got %f", m.QualityScore)
	}
	if len(m.Consolidated) != 2 {
		t.Errorf("expected 2 consolidated IDs, got %d", len(m.Consolidated))
	}
}

func TestMemoryMetadata_ToAny_Empty(t *testing.T) {
	m := MemoryMetadata{}
	result := m.ToAny()
	if result != nil {
		t.Errorf("expected nil for empty metadata, got %v", result)
	}
}

func TestMemoryMetadata_ToAny_NonEmpty(t *testing.T) {
	m := MemoryMetadata{Source: "user"}
	result := m.ToAny()
	if result == nil {
		t.Fatal("expected non-nil for non-empty metadata")
	}
	typed, ok := result.(MemoryMetadata)
	if !ok {
		t.Fatalf("expected MemoryMetadata, got %T", result)
	}
	if typed.Source != "user" {
		t.Errorf("expected Source=user, got %q", typed.Source)
	}
}
