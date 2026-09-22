package memory

import (
	"encoding/json"
	"reflect"
)

// sameStoredMemory reports whether persisting next would produce a revision
// byte-identical to what is already stored. It compares exactly the fields a
// revision carries — anything the temporal ledger does not record (CreatedAt is
// copied from the existing row, UpdatedAt is assigned by the store) is
// deliberately left out, so an unchanged re-save is recognised as the no-op it
// is.
func sameStoredMemory(existing, next Memory) bool {
	if existing.Content != next.Content ||
		existing.Category != next.Category ||
		existing.Source != next.Source ||
		existing.SourceID != next.SourceID ||
		existing.ContentHash != next.ContentHash {
		return false
	}
	if !sameProjectID(existing.ProjectID, next.ProjectID) {
		return false
	}
	if !sameKeywords(existing.Keywords, next.Keywords) {
		return false
	}
	return sameMetadata(existing.Metadata, next.Metadata)
}

func sameProjectID(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameKeywords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameMetadata compares two metadata values by their stored form. Metadata
// reaches this point either as the typed MemoryMetadata the save path builds or
// as the generic map a store read decodes, so the comparison goes through JSON
// — the representation the column actually holds — rather than comparing Go
// types that would never match across those two paths.
func sameMetadata(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	na, okA := normalizeMetadata(a)
	nb, okB := normalizeMetadata(b)
	if !okA || !okB {
		return false
	}
	return reflect.DeepEqual(na, nb)
}

func normalizeMetadata(v any) (any, bool) {
	if v == nil {
		return nil, true
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// deriveOutbox resolves the remote envelopes for a save exactly once.
// DeriveRemoteOutbox is supplied by callers that close over their own state
// (the MCP server flags auto-sync eligibility through it), so it must not be
// invoked twice per save — the no-op check and the write share this result.
func deriveOutbox(opts *DurableSaveOptions, m Memory) ([]RemoteOutboxSpec, error) {
	if opts == nil {
		return nil, nil
	}
	outbox := append([]RemoteOutboxSpec(nil), opts.RemoteOutbox...)
	if opts.DeriveRemoteOutbox == nil {
		return outbox, nil
	}
	derived, err := opts.DeriveRemoteOutbox(m)
	if err != nil {
		return nil, err
	}
	return append(outbox, derived...), nil
}

// withDerivedOutbox returns a copy of opts carrying already-resolved envelopes,
// so the write path consumes them instead of deriving them a second time.
func withDerivedOutbox(opts *DurableSaveOptions, outbox []RemoteOutboxSpec) *DurableSaveOptions {
	if opts == nil {
		return nil
	}
	resolved := *opts
	resolved.RemoteOutbox = outbox
	resolved.DeriveRemoteOutbox = nil
	return &resolved
}
