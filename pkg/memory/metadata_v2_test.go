package memory

import (
	"encoding/json"
	"testing"
	"time"
)

func TestV2_MarshalUnmarshalRoundTrip(t *testing.T) {
	original := MemoryMetadata{
		Source:      "bootstrap",
		MemoryType:  MemoryTypeSemantic,
		Kind:        "decision",
		Scope:       ScopeProject,
		Origin:      OriginBootstrap,
		Importance:  0.85,
		Pinned:      true,
		ExpiresAt:   "2026-12-31T23:59:59Z",
		Supersedes:  []string{"old-1", "old-2"},
		ContextTier: ContextTierL1,
		Confidence:  0.9,
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MemoryMetadata
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Source != original.Source {
		t.Errorf("Source: got %q, want %q", decoded.Source, original.Source)
	}
	if decoded.MemoryType != original.MemoryType {
		t.Errorf("MemoryType: got %q, want %q", decoded.MemoryType, original.MemoryType)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("Kind: got %q, want %q", decoded.Kind, original.Kind)
	}
	if decoded.Scope != original.Scope {
		t.Errorf("Scope: got %q, want %q", decoded.Scope, original.Scope)
	}
	if decoded.Origin != original.Origin {
		t.Errorf("Origin: got %q, want %q", decoded.Origin, original.Origin)
	}
	if decoded.Importance != original.Importance {
		t.Errorf("Importance: got %f, want %f", decoded.Importance, original.Importance)
	}
	if decoded.Pinned != original.Pinned {
		t.Errorf("Pinned: got %v, want %v", decoded.Pinned, original.Pinned)
	}
	if decoded.ExpiresAt != original.ExpiresAt {
		t.Errorf("ExpiresAt: got %q, want %q", decoded.ExpiresAt, original.ExpiresAt)
	}
	if decoded.ContextTier != original.ContextTier {
		t.Errorf("ContextTier: got %q, want %q", decoded.ContextTier, original.ContextTier)
	}
	if decoded.Confidence != original.Confidence {
		t.Errorf("Confidence: got %f, want %f", decoded.Confidence, original.Confidence)
	}
	if len(decoded.Supersedes) != len(original.Supersedes) {
		t.Fatalf("Supersedes len: got %d, want %d", len(decoded.Supersedes), len(original.Supersedes))
	}
	for i := range decoded.Supersedes {
		if decoded.Supersedes[i] != original.Supersedes[i] {
			t.Errorf("Supersedes[%d]: got %q, want %q", i, decoded.Supersedes[i], original.Supersedes[i])
		}
	}
}

func TestV2_ParseMetadata_FromMap(t *testing.T) {
	v := map[string]any{
		"source":       "dream",
		"memory_type":  "semantic",
		"kind":         "learning",
		"scope":        "project",
		"origin":       "dream",
		"importance":   0.7,
		"pinned":       true,
		"expires_at":   "2026-06-01T00:00:00Z",
		"supersedes":   []any{"s1", "s2"},
		"context_tier": "L2",
		"confidence":   0.95,
	}
	m := ParseMetadata(v)

	if m.Source != "dream" {
		t.Errorf("Source: got %q", m.Source)
	}
	if m.MemoryType != MemoryTypeSemantic {
		t.Errorf("MemoryType: got %q", m.MemoryType)
	}
	if m.Kind != "learning" {
		t.Errorf("Kind: got %q", m.Kind)
	}
	if m.Scope != ScopeProject {
		t.Errorf("Scope: got %q", m.Scope)
	}
	if m.Origin != OriginDream {
		t.Errorf("Origin: got %q", m.Origin)
	}
	if m.Importance != 0.7 {
		t.Errorf("Importance: got %f", m.Importance)
	}
	if !m.Pinned {
		t.Error("Pinned: got false, want true")
	}
	if len(m.Supersedes) != 2 || m.Supersedes[0] != "s1" {
		t.Errorf("Supersedes: got %v", m.Supersedes)
	}
	if m.ContextTier != ContextTierL2 {
		t.Errorf("ContextTier: got %q", m.ContextTier)
	}
	if m.Confidence != 0.95 {
		t.Errorf("Confidence: got %f", m.Confidence)
	}
}

func TestV2_ParseMetadata_PreservesUnknownKeys(t *testing.T) {
	v := map[string]any{
		"source":       "user",
		"memory_type":  "semantic",
		"custom_field": "custom_value",
		"another_key":  42,
	}
	m := ParseMetadata(v)

	if m.Source != "user" {
		t.Errorf("Source: got %q", m.Source)
	}
	if m.MemoryType != MemoryTypeSemantic {
		t.Errorf("MemoryType: got %q", m.MemoryType)
	}
	if len(m.Extra) != 2 {
		t.Fatalf("Extra: expected 2 keys, got %d", len(m.Extra))
	}
	if m.Extra["custom_field"] != "custom_value" {
		t.Errorf("Extra[custom_field]: got %v", m.Extra["custom_field"])
	}
	extraVal := m.Extra["another_key"]
	if extraVal == nil {
		t.Error("Extra[another_key]: got nil")
	}
}

func TestV2_ExtraRoundTrip(t *testing.T) {
	original := map[string]any{
		"source":      "import",
		"memory_type": "semantic",
		"tool_name":   "claude-code",
		"version":     "1.0",
	}
	m := ParseMetadata(original)
	result := m.ToAny()

	resultMap, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("ToAny with Extra: expected map[string]any, got %T", result)
	}
	if resultMap["source"] != "import" {
		t.Errorf("source: got %v", resultMap["source"])
	}
	if resultMap["memory_type"] != "semantic" {
		t.Errorf("memory_type: got %v", resultMap["memory_type"])
	}
	if resultMap["tool_name"] != "claude-code" {
		t.Errorf("tool_name (Extra): got %v", resultMap["tool_name"])
	}
	if resultMap["version"] != "1.0" {
		t.Errorf("version (Extra): got %v", resultMap["version"])
	}
}

func TestV2_ExtraDoesNotOverrideStructFields(t *testing.T) {
	m := MemoryMetadata{
		Source:     "user",
		MemoryType: MemoryTypeSemantic,
		Extra:      map[string]any{"source": "should-be-ignored"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["source"] != "user" {
		t.Errorf("struct field should take precedence: got %v", raw["source"])
	}
}

func TestV2_ToAny_NilWhenAllZero(t *testing.T) {
	m := MemoryMetadata{}
	if result := m.ToAny(); result != nil {
		t.Errorf("expected nil for all-zero v2 metadata, got %v", result)
	}
}

func TestV2_ToAny_NonNilWithV2Only(t *testing.T) {
	m := MemoryMetadata{MemoryType: MemoryTypeSemantic}
	if result := m.ToAny(); result == nil {
		t.Error("expected non-nil for v2-only metadata")
	}
}

func TestV2_ToAny_NonNilWithPinned(t *testing.T) {
	m := MemoryMetadata{Pinned: true}
	if result := m.ToAny(); result == nil {
		t.Error("expected non-nil when Pinned=true")
	}
}

func TestV2_Normalizers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) string
		in   string
		want string
	}{
		{"MemoryType_valid_semantic", NormalizeMemoryType, MemoryTypeSemantic, MemoryTypeSemantic},
		{"MemoryType_valid_episodic", NormalizeMemoryType, MemoryTypeEpisodic, MemoryTypeEpisodic},
		{"MemoryType_valid_operational", NormalizeMemoryType, MemoryTypeOperational, MemoryTypeOperational},
		{"MemoryType_invalid", NormalizeMemoryType, "unknown", ""},
		{"MemoryType_empty", NormalizeMemoryType, "", ""},

		{"Scope_valid_user", NormalizeScope, ScopeUser, ScopeUser},
		{"Scope_valid_project", NormalizeScope, ScopeProject, ScopeProject},
		{"Scope_valid_team", NormalizeScope, ScopeTeam, ScopeTeam},
		{"Scope_invalid", NormalizeScope, "global", ""},
		{"Scope_empty", NormalizeScope, "", ""},

		{"Origin_valid_manual", NormalizeOrigin, OriginManual, OriginManual},
		{"Origin_valid_bootstrap", NormalizeOrigin, OriginBootstrap, OriginBootstrap},
		{"Origin_valid_dream", NormalizeOrigin, OriginDream, OriginDream},
		{"Origin_valid_precompact", NormalizeOrigin, OriginPrecompact, OriginPrecompact},
		{"Origin_invalid", NormalizeOrigin, "auto", ""},

		{"ContextTier_valid_L0", NormalizeContextTier, ContextTierL0, ContextTierL0},
		{"ContextTier_valid_L1", NormalizeContextTier, ContextTierL1, ContextTierL1},
		{"ContextTier_valid_L2", NormalizeContextTier, ContextTierL2, ContextTierL2},
		{"ContextTier_invalid", NormalizeContextTier, "L3", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn(tt.in)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestV2_IsSemantic(t *testing.T) {
	m := MemoryMetadata{MemoryType: MemoryTypeSemantic}
	if !m.IsSemantic() {
		t.Error("expected IsSemantic=true for semantic type")
	}
	m = MemoryMetadata{MemoryType: MemoryTypeOperational}
	if m.IsSemantic() {
		t.Error("expected IsSemantic=false for operational type")
	}
	m = MemoryMetadata{}
	if m.IsSemantic() {
		t.Error("expected IsSemantic=false for empty type")
	}
}

func TestV2_IsOperational(t *testing.T) {
	m := MemoryMetadata{MemoryType: MemoryTypeOperational}
	if !m.IsOperational() {
		t.Error("expected IsOperational=true for operational type")
	}
	m = MemoryMetadata{MemoryType: MemoryTypeSemantic}
	if m.IsOperational() {
		t.Error("expected IsOperational=false for semantic type")
	}
}

func TestV2_IsExpired(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	m := MemoryMetadata{ExpiresAt: "2026-01-01T00:00:00Z"}
	if !m.IsExpired(now) {
		t.Error("expected expired for past date")
	}

	m = MemoryMetadata{ExpiresAt: "2027-01-01T00:00:00Z"}
	if m.IsExpired(now) {
		t.Error("expected not expired for future date")
	}

	m = MemoryMetadata{ExpiresAt: ""}
	if m.IsExpired(now) {
		t.Error("expected not expired for empty ExpiresAt")
	}

	m = MemoryMetadata{ExpiresAt: "not-a-date"}
	if m.IsExpired(now) {
		t.Error("expected not expired for unparseable date")
	}
}

func TestV2_IsRemoteSyncCandidate(t *testing.T) {
	tests := []struct {
		name string
		meta MemoryMetadata
		want bool
	}{
		{
			name: "semantic_project",
			meta: MemoryMetadata{MemoryType: MemoryTypeSemantic, Scope: ScopeProject},
			want: true,
		},
		{
			name: "semantic_team",
			meta: MemoryMetadata{MemoryType: MemoryTypeSemantic, Scope: ScopeTeam},
			want: true,
		},
		{
			name: "semantic_user",
			meta: MemoryMetadata{MemoryType: MemoryTypeSemantic, Scope: ScopeUser},
			want: false,
		},
		{
			name: "episodic_project",
			meta: MemoryMetadata{MemoryType: MemoryTypeEpisodic, Scope: ScopeProject},
			want: false,
		},
		{
			name: "operational_project",
			meta: MemoryMetadata{MemoryType: MemoryTypeOperational, Scope: ScopeProject},
			want: false,
		},
		{
			name: "precompact_kind",
			meta: MemoryMetadata{MemoryType: MemoryTypeSemantic, Scope: ScopeProject, Kind: "precompact"},
			want: false,
		},
		{
			name: "precompact_origin",
			meta: MemoryMetadata{MemoryType: MemoryTypeSemantic, Scope: ScopeProject, Origin: OriginPrecompact},
			want: false,
		},
		{
			name: "empty_metadata",
			meta: MemoryMetadata{},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.meta.IsRemoteSyncCandidate()
			if got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestV2_InferDefaultsFromCategory(t *testing.T) {
	tests := []struct {
		category   string
		hasProject bool
		wantType   string
		wantScope  string
	}{
		{"fact", true, MemoryTypeSemantic, ScopeProject},
		{"fact", false, MemoryTypeSemantic, ScopeUser},
		{"decision", true, MemoryTypeSemantic, ScopeProject},
		{"decision", false, MemoryTypeSemantic, ScopeUser},
		{"learning", true, MemoryTypeSemantic, ScopeProject},
		{"learning", false, MemoryTypeSemantic, ScopeUser},
		{"plan", true, MemoryTypeSemantic, ScopeProject},
		{"plan", false, MemoryTypeSemantic, ScopeUser},
		{"summary", true, MemoryTypeSemantic, ScopeProject},
		{"summary", false, MemoryTypeSemantic, ScopeUser},
		{"event", true, MemoryTypeEpisodic, ScopeUser},
		{"event", false, MemoryTypeEpisodic, ScopeUser},
		{"preference", true, MemoryTypeSemantic, ScopeUser},
		{"preference", false, MemoryTypeSemantic, ScopeUser},
	}
	for _, tt := range tests {
		t.Run(tt.category+"_project_"+string(rune('0'+boolToInt(tt.hasProject))), func(t *testing.T) {
			m := InferDefaultsFromCategory(tt.category, tt.hasProject)
			if m.MemoryType != tt.wantType {
				t.Errorf("MemoryType: got %q, want %q", m.MemoryType, tt.wantType)
			}
			if m.Scope != tt.wantScope {
				t.Errorf("Scope: got %q, want %q", m.Scope, tt.wantScope)
			}
		})
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestV2_MixedV1V2RoundTrip(t *testing.T) {
	original := MemoryMetadata{
		Source:          "dream",
		SessionID:       "sess_42",
		Consolidated:    []string{"a", "b"},
		QualityScore:    0.88,
		PreferenceScope: PreferenceScopeUser,
		MemoryType:      MemoryTypeSemantic,
		Kind:            "decision",
		Scope:           ScopeProject,
		Origin:          OriginDream,
		Importance:      0.9,
		Supersedes:      []string{"old"},
		ContextTier:     ContextTierL1,
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MemoryMetadata
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Source != original.Source {
		t.Errorf("Source: got %q, want %q", decoded.Source, original.Source)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID: got %q, want %q", decoded.SessionID, original.SessionID)
	}
	if decoded.PreferenceScope != original.PreferenceScope {
		t.Errorf("PreferenceScope: got %q, want %q", decoded.PreferenceScope, original.PreferenceScope)
	}
	if decoded.MemoryType != original.MemoryType {
		t.Errorf("MemoryType: got %q, want %q", decoded.MemoryType, original.MemoryType)
	}
	if decoded.Scope != original.Scope {
		t.Errorf("Scope: got %q, want %q", decoded.Scope, original.Scope)
	}
}

func TestV2_ExistingTestsStillPass_V1FieldsUnchanged(t *testing.T) {
	m := MemoryMetadata{
		Source:          "auto_capture",
		SessionID:       "sess_123",
		Consolidated:    []string{"id1", "id2"},
		DreamVersion:    "dream_v3",
		CaptureReason:   "high relevance",
		QualityScore:    0.95,
		PreferenceScope: PreferenceScopeProject,
	}

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if raw["source"] != "auto_capture" {
		t.Errorf("source: got %v", raw["source"])
	}
	if raw["session_id"] != "sess_123" {
		t.Errorf("session_id: got %v", raw["session_id"])
	}
	if raw["preference_scope"] != "project" {
		t.Errorf("preference_scope: got %v", raw["preference_scope"])
	}

	var decoded MemoryMetadata
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal struct: %v", err)
	}
	if decoded.Source != "auto_capture" {
		t.Errorf("Source: got %q", decoded.Source)
	}
	if decoded.PreferenceScope != "project" {
		t.Errorf("PreferenceScope: got %q", decoded.PreferenceScope)
	}
}

func TestV2_ParseMetadata_DirectType(t *testing.T) {
	original := MemoryMetadata{
		Source:     "import",
		MemoryType: MemoryTypeSemantic,
		Scope:      ScopeProject,
	}
	m := ParseMetadata(original)
	if m.Source != original.Source {
		t.Errorf("Source: got %q, want %q", m.Source, original.Source)
	}
	if m.MemoryType != original.MemoryType {
		t.Errorf("MemoryType: got %q, want %q", m.MemoryType, original.MemoryType)
	}
	if m.Scope != original.Scope {
		t.Errorf("Scope: got %q, want %q", m.Scope, original.Scope)
	}
	if len(m.Extra) != 0 {
		t.Errorf("Extra: expected empty, got %v", m.Extra)
	}
}

func TestV2_HandoffMetadata(t *testing.T) {
	m := HandoffMetadata("project", "2026-12-31T23:59:59Z")
	if m.MemoryType != MemoryTypeOperational {
		t.Errorf("MemoryType: got %q", m.MemoryType)
	}
	if m.Kind != "handoff" {
		t.Errorf("Kind: got %q", m.Kind)
	}
	if m.Origin != OriginHandoff {
		t.Errorf("Origin: got %q", m.Origin)
	}
	if m.Scope != ScopeProject {
		t.Errorf("Scope: got %q", m.Scope)
	}
	if m.ContextTier != ContextTierL1 {
		t.Errorf("ContextTier: got %q", m.ContextTier)
	}
	if m.ExpiresAt != "2026-12-31T23:59:59Z" {
		t.Errorf("ExpiresAt: got %q", m.ExpiresAt)
	}
}

func TestV2_PreCompactMetadata(t *testing.T) {
	m := PreCompactMetadata("user", "2026-06-01T00:00:00Z")
	if m.MemoryType != MemoryTypeOperational {
		t.Errorf("MemoryType: got %q", m.MemoryType)
	}
	if m.Kind != "precompact" {
		t.Errorf("Kind: got %q", m.Kind)
	}
	if m.Origin != OriginPrecompact {
		t.Errorf("Origin: got %q", m.Origin)
	}
	if m.Scope != ScopeUser {
		t.Errorf("Scope: got %q", m.Scope)
	}
}

func TestV2_BootstrapMetadata(t *testing.T) {
	m := BootstrapMetadata(0.85)
	if m.MemoryType != MemoryTypeSemantic {
		t.Errorf("MemoryType: got %q", m.MemoryType)
	}
	if m.Origin != OriginBootstrap {
		t.Errorf("Origin: got %q", m.Origin)
	}
	if m.Scope != ScopeProject {
		t.Errorf("Scope: got %q", m.Scope)
	}
	if m.Confidence != 0.85 {
		t.Errorf("Confidence: got %f", m.Confidence)
	}
	if m.ContextTier != ContextTierL0 {
		t.Errorf("ContextTier: got %q", m.ContextTier)
	}
}

func TestV2_ParseMetadataFromJSON(t *testing.T) {
	tests := []struct {
		name    string
		jsonStr string
		want    MemoryMetadata
	}{
		{
			name:    "empty_string",
			jsonStr: "",
			want:    MemoryMetadata{},
		},
		{
			name:    "null_literal",
			jsonStr: "null",
			want:    MemoryMetadata{},
		},
		{
			name:    "invalid_json",
			jsonStr: "{broken",
			want:    MemoryMetadata{},
		},
		{
			name:    "valid_v2_metadata",
			jsonStr: `{"memory_type":"semantic","kind":"decision","scope":"project","origin":"bootstrap","importance":0.9,"pinned":true,"expires_at":"2027-01-01T00:00:00Z","supersedes":["old1"],"context_tier":"L0","confidence":0.85}`,
			want: MemoryMetadata{
				MemoryType:  MemoryTypeSemantic,
				Kind:        "decision",
				Scope:       ScopeProject,
				Origin:      OriginBootstrap,
				Importance:  0.9,
				Pinned:      true,
				ExpiresAt:   "2027-01-01T00:00:00Z",
				Supersedes:  []string{"old1"},
				ContextTier: ContextTierL0,
				Confidence:  0.85,
			},
		},
		{
			name:    "partial_v2_only_pinned",
			jsonStr: `{"pinned":true}`,
			want:    MemoryMetadata{Pinned: true},
		},
		{
			name:    "v1_only_fields",
			jsonStr: `{"source":"auto_capture","session_id":"sess_42"}`,
			want:    MemoryMetadata{Source: "auto_capture", SessionID: "sess_42"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseMetadataFromJSON(tt.jsonStr)
			if got.MemoryType != tt.want.MemoryType {
				t.Errorf("MemoryType: got %q, want %q", got.MemoryType, tt.want.MemoryType)
			}
			if got.Kind != tt.want.Kind {
				t.Errorf("Kind: got %q, want %q", got.Kind, tt.want.Kind)
			}
			if got.Scope != tt.want.Scope {
				t.Errorf("Scope: got %q, want %q", got.Scope, tt.want.Scope)
			}
			if got.Origin != tt.want.Origin {
				t.Errorf("Origin: got %q, want %q", got.Origin, tt.want.Origin)
			}
			if got.Importance != tt.want.Importance {
				t.Errorf("Importance: got %f, want %f", got.Importance, tt.want.Importance)
			}
			if got.Pinned != tt.want.Pinned {
				t.Errorf("Pinned: got %v, want %v", got.Pinned, tt.want.Pinned)
			}
			if got.ExpiresAt != tt.want.ExpiresAt {
				t.Errorf("ExpiresAt: got %q, want %q", got.ExpiresAt, tt.want.ExpiresAt)
			}
			if got.ContextTier != tt.want.ContextTier {
				t.Errorf("ContextTier: got %q, want %q", got.ContextTier, tt.want.ContextTier)
			}
			if got.Confidence != tt.want.Confidence {
				t.Errorf("Confidence: got %f, want %f", got.Confidence, tt.want.Confidence)
			}
			if got.Source != tt.want.Source {
				t.Errorf("Source: got %q, want %q", got.Source, tt.want.Source)
			}
			if got.SessionID != tt.want.SessionID {
				t.Errorf("SessionID: got %q, want %q", got.SessionID, tt.want.SessionID)
			}
			if len(got.Supersedes) != len(tt.want.Supersedes) {
				t.Errorf("Supersedes len: got %d, want %d", len(got.Supersedes), len(tt.want.Supersedes))
			}
		})
	}
}
