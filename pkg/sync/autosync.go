package sync

import "github.com/jholhewres/anchored/pkg/memory"

// ClassifyForAutoSync decides whether a single memory may leave the machine for
// the auto write-through path. It runs the same classification as the
// preview/sync path (secrets, local paths, user scope, episodic/precompact, low
// quality, blocked categories). When syncable it returns the REDACTED content
// (home paths rewritten to <user-home>, etc.) — callers MUST push this, never
// the original, so redactions aren't bypassed. ok is false for anything not
// classified Syncable.
func ClassifyForAutoSync(m memory.Memory, projectRoot string) (content string, ok bool) {
	content, _, ok = ClassifyForRemote(m, projectRoot)
	return content, ok
}

// ClassifyForRemote is ClassifyForAutoSync for any save that sends a memory to
// a remote, automatic or asked for by name: a save that names its remote
// gets no looser rules, so a user-scope or personal memory never leaves the
// machine either way. reason is the preview's reason for a memory it blocks
// ("user_scope_blocked", "secret_pattern", ...).
func ClassifyForRemote(m memory.Memory, projectRoot string) (content, reason string, ok bool) {
	res := ClassifyForPreview([]Memory{ToSyncMemory(m)}, projectRoot)
	if len(res.Items) != 1 {
		return "", "unclassified", false
	}
	item := res.Items[0]
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
