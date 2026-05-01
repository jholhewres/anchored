package memory

import "encoding/json"

// MemoryMetadata provides structured metadata for memories.
// Stored as JSON in the Memory.Metadata field (which is `any` for backward compat).
type MemoryMetadata struct {
	Source        string   `json:"source,omitempty"`         // "user", "auto_capture", "dream", "import", "precompact"
	SessionID     string   `json:"session_id,omitempty"`     // Source session that created this memory
	Consolidated  []string `json:"consolidated,omitempty"`   // IDs of memories merged into this one
	DreamVersion  string   `json:"dream_version,omitempty"`  // Dream run ID that processed this
	CaptureReason string   `json:"capture_reason,omitempty"` // Why this was captured
	QualityScore  float64  `json:"quality_score,omitempty"`  // Auto-capture quality score
}

// ToAny converts MemoryMetadata to the `any` type expected by Memory.Metadata.
func (m MemoryMetadata) ToAny() any {
	// Return nil if all fields are zero
	if m.Source == "" && m.SessionID == "" && len(m.Consolidated) == 0 && m.DreamVersion == "" && m.CaptureReason == "" && m.QualityScore == 0 {
		return nil
	}
	return m
}

// MarshalJSON implements json.Marshaler using an alias to avoid infinite recursion.
func (m MemoryMetadata) MarshalJSON() ([]byte, error) {
	type alias MemoryMetadata
	return json.Marshal(alias(m))
}

// ParseMetadata parses an `any` (from Memory.Metadata) into MemoryMetadata.
// Returns zero-value MemoryMetadata if input is nil or unparseable.
func ParseMetadata(v any) MemoryMetadata {
	if v == nil {
		return MemoryMetadata{}
	}

	// If already MemoryMetadata, return as-is
	if m, ok := v.(MemoryMetadata); ok {
		return m
	}

	// If map, marshal and unmarshal
	b, err := json.Marshal(v)
	if err != nil {
		return MemoryMetadata{}
	}
	var m MemoryMetadata
	if err := json.Unmarshal(b, &m); err != nil {
		return MemoryMetadata{}
	}
	return m
}
