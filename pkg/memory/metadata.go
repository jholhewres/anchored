package memory

import (
	"encoding/json"
	"strings"
	"time"
)

// v1 scope constants (existing)
const (
	PreferenceScopeUser    = "user"
	PreferenceScopeProject = "project"
	PreferenceScopeTeam    = "team"
)

// v2 lifecycle constants
const (
	MemoryTypeSemantic    = "semantic"
	MemoryTypeEpisodic    = "episodic"
	MemoryTypeOperational = "operational"

	ScopeUser    = "user"
	ScopeProject = "project"
	ScopeTeam    = "team"

	OriginManual     = "manual"
	OriginHook       = "hook"
	OriginBootstrap  = "bootstrap"
	OriginDream      = "dream"
	OriginRemote     = "remote"
	OriginHandoff    = "handoff"
	OriginPrecompact = "precompact"
	OriginImport     = "import"

	ContextTierL0 = "L0"
	ContextTierL1 = "L1"
	ContextTierL2 = "L2"

	CurationStatusLowSignal = "low_signal"

	// CurationRule values explaining a low_signal demotion.
	CurationRuleQuality      = "quality"      // mechanical quality score below threshold
	CurationRuleNeverUsed    = "never_used"   // injected many times, never drawn on
	CurationRuleConsolidated = "consolidated" // raw member of a synthesized cluster

	// NeverUsedInjectionFloor is how many injections a memory gets before a
	// zero used_count demotes it (Feature D). Generous on purpose: ten chances
	// to prove useful before the feedback loop pulls it out of rotation.
	NeverUsedInjectionFloor = 10

	// RemoteQualityThreshold: minimum quality_score for remote sync eligibility.
	// Referenced by preview, IsRemoteSyncCandidate, and hybrid search demotion.
	RemoteQualityThreshold = 0.55

	// QualityScorerVersion identifies the current ScoreQuality formula. It is
	// stamped onto metadata whenever a memory is (re)curated. The serve-time
	// worker and `curation reconcile` treat any memory whose scorer_version is
	// missing or lower than this as a candidate, so formula changes re-flow
	// through the whole corpus instead of only touching brand-new memories.
	// v3: usage-feedback demotion (never_used) joined the recurate pass.
	QualityScorerVersion = 3
)

// MemoryMetadata provides structured metadata for memories.
// Stored as JSON in the Memory.Metadata field (which is `any` for backward compat).
type MemoryMetadata struct {
	// v1 fields (existing, unchanged)
	Source        string   `json:"source,omitempty"`         // "user", "auto_capture", "dream", "import", "precompact"
	SessionID     string   `json:"session_id,omitempty"`     // Source session that created this memory
	Consolidated  []string `json:"consolidated,omitempty"`   // IDs of memories merged into this one
	DreamVersion  string   `json:"dream_version,omitempty"`  // Dream run ID that processed this
	CaptureReason string   `json:"capture_reason,omitempty"` // Why this was captured
	QualityScore  float64  `json:"quality_score,omitempty"`  // Auto-capture quality score
	// PreferenceScope distinguishes personal preferences from project/team conventions.
	// Valid values: "user", "project", "team". Empty means unknown/non-preference.
	PreferenceScope string `json:"preference_scope,omitempty"`

	// v2 lifecycle fields (all optional, zero-value means unset)
	MemoryType     string   `json:"memory_type,omitempty"`  // "semantic", "episodic", "operational"
	Kind           string   `json:"kind,omitempty"`         // "fact", "decision", "learning", "summary", "rule", "handoff", "precompact"
	Scope          string   `json:"scope,omitempty"`        // "user", "project", "team"
	Origin         string   `json:"origin,omitempty"`       // "manual", "hook", "bootstrap", "dream", "remote", "handoff", "precompact", "import"
	Importance     float64  `json:"importance,omitempty"`   // 0.0..1.0 ranking and retention hint
	Pinned         bool     `json:"pinned,omitempty"`       // exempt from retention and demotion
	ExpiresAt      string   `json:"expires_at,omitempty"`   // RFC3339 timestamp for operational/episodic TTL
	Supersedes     []string `json:"supersedes,omitempty"`   // IDs of memories this item replaces
	ContextTier    string   `json:"context_tier,omitempty"` // "L0", "L1", "L2" context stack hint
	Confidence     float64  `json:"confidence,omitempty"`   // 0.0..1.0 for bootstrap/import/inferred items
	CurationStatus string   `json:"curation_status,omitempty"`
	// CurationRule records WHY curation_status was set ("quality" or
	// "never_used"), so the lift conditions don't conflict: a quality demotion
	// lifts when the score recovers; a never_used demotion lifts when the
	// memory is finally used. INVARIANT: must be cleared together with
	// CurationStatus — never one without the other (RecurateMetadata is the
	// canonical writer for both).
	CurationRule string `json:"curation_rule,omitempty"`
	// ScorerVersion records which ScoreQuality formula last curated this memory.
	// Drives re-curation: a value below QualityScorerVersion means stale. Also
	// acts as the "has been scored" marker so a legitimate quality_score of 0
	// (omitempty would otherwise drop it) does not look like an unscored memory.
	ScorerVersion int `json:"scorer_version,omitempty"`

	// Usage-feedback fields (Feature D). InjectedCount/LastInjectedAt are
	// written by the UserPromptSubmit hook when a memory is put in front of
	// the model; UsedCount/LastUsedAt by the Stop hook when the turn's text
	// actually drew on it. RecurateMetadata consumes the pair to demote
	// always-injected-never-used memories.
	InjectedCount  int    `json:"injected_count,omitempty"`
	UsedCount      int    `json:"used_count,omitempty"`
	LastInjectedAt string `json:"last_injected_at,omitempty"`
	LastUsedAt     string `json:"last_used_at,omitempty"`

	// Extra preserves unknown metadata keys from raw maps that are not
	// represented in this struct. Not serialized directly; merged during
	// MarshalJSON and ToAny.
	Extra map[string]any `json:"-"`
}

// isZero returns true when all fields (v1 + v2) are at their zero values
// and no extra keys are preserved.
func (m MemoryMetadata) isZero() bool {
	return m.Source == "" &&
		m.SessionID == "" &&
		len(m.Consolidated) == 0 &&
		m.DreamVersion == "" &&
		m.CaptureReason == "" &&
		m.QualityScore == 0 &&
		m.PreferenceScope == "" &&
		m.MemoryType == "" &&
		m.Kind == "" &&
		m.Scope == "" &&
		m.Origin == "" &&
		m.Importance == 0 &&
		!m.Pinned &&
		m.ExpiresAt == "" &&
		len(m.Supersedes) == 0 &&
		m.ContextTier == "" &&
		m.Confidence == 0 &&
		m.CurationStatus == "" &&
		m.CurationRule == "" &&
		m.ScorerVersion == 0 &&
		m.InjectedCount == 0 &&
		m.UsedCount == 0 &&
		m.LastInjectedAt == "" &&
		m.LastUsedAt == "" &&
		len(m.Extra) == 0
}

// ToAny converts MemoryMetadata to the `any` type expected by Memory.Metadata.
// Returns nil when all fields are zero.
func (m MemoryMetadata) ToAny() any {
	if m.isZero() {
		return nil
	}
	if len(m.Extra) == 0 {
		return m
	}
	b, err := m.MarshalJSON()
	if err != nil {
		return m
	}
	var merged map[string]any
	if err := json.Unmarshal(b, &merged); err != nil {
		return m
	}
	for k, v := range m.Extra {
		merged[k] = v
	}
	return merged
}

// MarshalJSON implements json.Marshaler using an alias to avoid infinite recursion.
// Merges Extra keys into the JSON output.
func (m MemoryMetadata) MarshalJSON() ([]byte, error) {
	type alias MemoryMetadata
	b, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return b, nil
	}
	var merged map[string]any
	if err := json.Unmarshal(b, &merged); err != nil {
		return b, nil
	}
	for k, v := range m.Extra {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

// ParseMetadata parses an `any` (from Memory.Metadata) into MemoryMetadata.
// Returns zero-value MemoryMetadata if input is nil or unparseable.
// Unknown keys from raw maps are preserved in the Extra field.
func ParseMetadata(v any) MemoryMetadata {
	if v == nil {
		return MemoryMetadata{}
	}

	if m, ok := v.(MemoryMetadata); ok {
		return m
	}

	if raw, ok := v.(map[string]any); ok {
		b, err := json.Marshal(raw)
		if err != nil {
			return MemoryMetadata{}
		}
		var m MemoryMetadata
		if err := json.Unmarshal(b, &m); err != nil {
			return MemoryMetadata{}
		}

		knownKeys := map[string]bool{
			"source": true, "session_id": true, "consolidated": true,
			"dream_version": true, "capture_reason": true, "quality_score": true,
			"preference_scope": true,
			"memory_type":      true, "kind": true, "scope": true, "origin": true,
			"importance": true, "pinned": true, "expires_at": true,
			"supersedes": true, "context_tier": true, "confidence": true,
			"curation_status": true, "curation_rule": true, "scorer_version": true,
			"injected_count": true, "used_count": true,
			"last_injected_at": true, "last_used_at": true,
		}
		for k, val := range raw {
			if !knownKeys[k] {
				if m.Extra == nil {
					m.Extra = make(map[string]any)
				}
				m.Extra[k] = val
			}
		}
		return m
	}

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

func NormalizePreferenceScope(scope string) string {
	switch scope {
	case PreferenceScopeProject:
		return PreferenceScopeProject
	case PreferenceScopeTeam:
		return PreferenceScopeTeam
	default:
		return PreferenceScopeUser
	}
}

func NormalizeMemoryType(t string) string {
	switch t {
	case MemoryTypeSemantic, MemoryTypeEpisodic, MemoryTypeOperational:
		return t
	default:
		return ""
	}
}

func NormalizeScope(s string) string {
	switch s {
	case ScopeUser, ScopeProject, ScopeTeam:
		return s
	default:
		return ""
	}
}

func NormalizeOrigin(o string) string {
	switch o {
	case OriginManual, OriginHook, OriginBootstrap, OriginDream,
		OriginRemote, OriginHandoff, OriginPrecompact, OriginImport:
		return o
	default:
		return ""
	}
}

func NormalizeContextTier(tier string) string {
	switch tier {
	case ContextTierL0, ContextTierL1, ContextTierL2:
		return tier
	default:
		return ""
	}
}

func (m MemoryMetadata) IsSemantic() bool {
	return m.MemoryType == MemoryTypeSemantic
}

func (m MemoryMetadata) IsOperational() bool {
	return m.MemoryType == MemoryTypeOperational
}

// IsExpired returns true if ExpiresAt is set and before the given time.
// Returns false if ExpiresAt is empty or unparseable.
func (m MemoryMetadata) IsExpired(now time.Time) bool {
	if m.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, m.ExpiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// IsRemoteSyncCandidate returns true when this memory is safe to push to a
// shared remote. Uses v2 metadata when available; conservative when absent.
func (m MemoryMetadata) IsRemoteSyncCandidate() bool {
	// User-scoped data never syncs
	if m.Scope == ScopeUser {
		return false
	}
	if m.CurationStatus == CurationStatusLowSignal {
		return false
	}
	if m.QualityScore > 0 && m.QualityScore < RemoteQualityThreshold && !m.Pinned {
		return false
	}
	// Episodic never syncs
	if m.MemoryType == MemoryTypeEpisodic {
		return false
	}
	// PreCompact never syncs
	if m.Kind == "precompact" || m.Origin == OriginPrecompact {
		return false
	}
	// Must be semantic with project or team scope
	if m.MemoryType == MemoryTypeSemantic && (m.Scope == ScopeProject || m.Scope == ScopeTeam) {
		return true
	}
	// When v2 metadata is absent, conservative: not a candidate
	// Callers should fall back to category-based logic.
	return false
}

// WithSaveScope applies the scope a save was given. For a preference it is
// the preference's scope. For any other category, "project" and "team" leave
// the scope to be inferred from the project, and any other value narrows the
// memory to the personal scope, which never leaves the machine: a scope that
// is mistyped, or spelled to look like "user", fails closed, as the
// preference branch does.
func WithSaveScope(metadata any, category, scope string) any {
	if category != "preference" {
		m := ParseMetadata(metadata)
		s := strings.ToLower(strings.TrimSpace(scope))
		personal := s != "" && s != ScopeProject && s != ScopeTeam
		if m.PreferenceScope == "" && !personal {
			return metadata
		}
		m.PreferenceScope = ""
		if personal {
			m.Scope = ScopeUser
		}
		return m.ToAny()
	}
	m := ParseMetadata(metadata)
	m.PreferenceScope = NormalizePreferenceScope(scope)
	return m.ToAny()
}

// InferDefaultsFromCategory returns a MemoryMetadata with default v2 lifecycle
// fields inferred from the given category. This is used when no v2 metadata is
// present on a memory.
func InferDefaultsFromCategory(category string, hasProject bool) MemoryMetadata {
	m := MemoryMetadata{}

	switch category {
	case "fact", "decision", "learning":
		m.MemoryType = MemoryTypeSemantic
		if hasProject {
			m.Scope = ScopeProject
		} else {
			m.Scope = ScopeUser
		}
	case "plan", "summary":
		m.MemoryType = MemoryTypeSemantic
		if hasProject {
			m.Scope = ScopeProject
		} else {
			m.Scope = ScopeUser
		}
	case "event":
		m.MemoryType = MemoryTypeEpisodic
		m.Scope = ScopeUser
	case "preference":
		m.MemoryType = MemoryTypeSemantic
		m.Scope = ScopeUser
	default:
		// A category outside the known ones still gets a scope, so the
		// remote classifier never sees a memory with none: personal
		// outside a project.
		if hasProject {
			m.Scope = ScopeProject
		} else {
			m.Scope = ScopeUser
		}
	}

	return m
}

func ParseMetadataFromJSON(jsonStr string) MemoryMetadata {
	if jsonStr == "" || jsonStr == "null" {
		return MemoryMetadata{}
	}
	var m MemoryMetadata
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		return MemoryMetadata{}
	}
	return m
}

func HandoffMetadata(scope, expiresAt string) MemoryMetadata {
	return MemoryMetadata{
		MemoryType:  MemoryTypeOperational,
		Kind:        "handoff",
		Origin:      OriginHandoff,
		Scope:       NormalizeScope(scope),
		ExpiresAt:   expiresAt,
		ContextTier: ContextTierL1,
	}
}

func PreCompactMetadata(scope, expiresAt string) MemoryMetadata {
	return MemoryMetadata{
		MemoryType:  MemoryTypeOperational,
		Kind:        "precompact",
		Origin:      OriginPrecompact,
		Scope:       NormalizeScope(scope),
		ExpiresAt:   expiresAt,
		ContextTier: ContextTierL1,
	}
}

func BootstrapMetadata(confidence float64) MemoryMetadata {
	return MemoryMetadata{
		MemoryType:  MemoryTypeSemantic,
		Origin:      OriginBootstrap,
		Scope:       ScopeProject,
		Confidence:  confidence,
		ContextTier: ContextTierL0,
	}
}
