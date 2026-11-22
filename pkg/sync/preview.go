package sync

import (
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"
)

func ClassifyForPreview(memories []Memory, projectRoot string) PreviewResult {
	result := PreviewResult{
		Total: len(memories),
	}

	for i := range memories {
		item := classifyMemory(memories[i], projectRoot, true)
		switch item.Classification {
		case ClassificationSyncable:
			result.Syncable++
		case ClassificationBlocked:
			result.Blocked++
		case ClassificationNeedsReview:
			result.NeedsReview++
		}
		result.Items = append(result.Items, item)
	}

	return result
}

// classifyMemory is one memory's classification. quality says whether the
// quality gates (low_signal, low_quality) apply; every privacy rule always
// does.
func classifyMemory(mem Memory, projectRoot string, quality bool) PreviewItem {
	m := &mem
	item := PreviewItem{Memory: *m}

	meta := toMap(m.Metadata)
	filterResult := RemoteSafetyFilter(m.Content, meta, projectRoot)
	item.Memory.Content = filterResult.Content

	scope := m.PreferenceScope
	if scope == "" {
		scope = preferenceScope(meta)
	}

	lifecycle := memory.ParseMetadata(m.Metadata)

	switch {
	case filterResult.Blocked:
		item.Classification = ClassificationBlocked
		item.Reason = violationReason(filterResult.Violations)

	case m.SyncOrigin != "" && m.SyncOrigin != "local":
		item.Classification = ClassificationNeedsReview
		item.Reason = "already_synced"

	case m.SyncDirty:
		item.Classification = ClassificationNeedsReview
		item.Reason = "pending_changes"

	case lifecycle.MemoryType == memory.MemoryTypeEpisodic:
		item.Classification = ClassificationBlocked
		item.Reason = "episodic_local_only"
	case lifecycle.Kind == "precompact":
		item.Classification = ClassificationBlocked
		item.Reason = "precompact_local_only"
	case lifecycle.Origin == memory.OriginPrecompact:
		item.Classification = ClassificationBlocked
		item.Reason = "precompact_origin"
	case lifecycle.MemoryType == memory.MemoryTypeOperational && lifecycle.Kind != "handoff":
		item.Classification = ClassificationBlocked
		item.Reason = "operational_local_only"

	case lifecycle.Scope == memory.ScopeUser:
		item.Classification = ClassificationBlocked
		item.Reason = "user_scope_blocked"

	case quality && lifecycle.CurationStatus == memory.CurationStatusLowSignal:
		item.Classification = ClassificationBlocked
		item.Reason = "low_signal"

	case quality && lifecycle.QualityScore > 0 && lifecycle.QualityScore < memory.RemoteQualityThreshold && !lifecycle.Pinned:
		item.Classification = ClassificationBlocked
		item.Reason = "low_quality"

	case scope == "user":
		item.Classification = ClassificationBlocked
		item.Reason = "personal_preference"

	case strings.EqualFold(m.Category, "event") || strings.EqualFold(m.Category, "preference"):
		item.Classification = ClassificationBlocked
		item.Reason = "category_remote_blocked"

	case lifecycle.IsRemoteSyncCandidate():
		item.Classification = ClassificationSyncable
		item.Reason = ""

	default:
		item.Classification = ClassificationSyncable
		item.Reason = ""
	}

	return item
}

func toMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func preferenceScope(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	s, ok := meta["scope"].(string)
	if !ok {
		return ""
	}
	return s
}

func violationReason(violations []RemoteSafetyViolation) string {
	if len(violations) == 0 {
		return "unknown"
	}
	return violations[0].Reason
}
