package sync

import "github.com/jholhewres/anchored/pkg/memory"

// ClassifyForRemote decides whether a memory may leave the machine on the
// automatic paths (auto-sync, the outbox), with the same classification as
// the preview and `remote sync`: secrets, local paths, user scope,
// episodic/operational/precompact, low quality, blocked categories. When it
// may, content is the REDACTED form (project paths made relative, secrets
// redacted): callers MUST send that, never the original. reason is the
// preview's reason for one it blocks ("user_scope_blocked",
// "secret_pattern", ...).
func ClassifyForRemote(m memory.Memory, projectRoot string) (content, reason string, ok bool) {
	return classifyForRemote(m, projectRoot, true)
}

// ClassifyForNamedRemote is ClassifyForRemote for a save that names its
// remote: every privacy rule applies, but not the quality gates (low_signal,
// low_quality), since the person asked for this memory to go.
func ClassifyForNamedRemote(m memory.Memory, projectRoot string) (content, reason string, ok bool) {
	return classifyForRemote(m, projectRoot, false)
}

func classifyForRemote(m memory.Memory, projectRoot string, quality bool) (content, reason string, ok bool) {
	item := classifyMemory(ToSyncMemory(m), projectRoot, quality)
	if item.Classification != ClassificationSyncable {
		return "", item.Reason, false
	}
	return item.Memory.Content, "", true
}

// ToSyncMemory converts a local memory into the sync wire model. Exported so
// both the CLI sync command and the MCP auto write-through path share one
// mapping.
func ToSyncMemory(m memory.Memory) Memory {
	return Memory{
		ID:               m.ID,
		Category:         m.Category,
		Content:          m.Content,
		ProjectID:        m.ProjectID,
		Source:           m.Source,
		SyncOrigin:       m.SyncOrigin,
		SyncDirty:        m.SyncDirty,
		RemoteProjectKey: m.RemoteProjectKey,
		PreferenceScope:  preferenceScopeFromMetadata(m.Metadata),
		Metadata:         m.Metadata,
	}
}

func preferenceScopeFromMetadata(v any) string {
	switch m := v.(type) {
	case memory.MemoryMetadata:
		return m.PreferenceScope
	case map[string]any:
		s, _ := m["scope"].(string)
		return s
	default:
		return memory.ParseMetadata(v).PreferenceScope
	}
}
