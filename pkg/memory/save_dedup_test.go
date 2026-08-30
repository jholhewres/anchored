package memory

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// contentHash is verbatim (no normalization) to stay byte-compatible with the
// sync protocol and older clients. Case/whitespace variants therefore hash
// DIFFERENTLY here and are instead folded by the near-duplicate merge
// (TestFindNearDuplicate). This test pins that contract.
func TestContentHash_Verbatim(t *testing.T) {
	if contentHash("We use Postgres.") == contentHash("we use postgres.") {
		t.Error("contentHash must be verbatim (case-sensitive) for sync compatibility")
	}
	if contentHash("we use postgres") != contentHash("we use postgres") {
		t.Error("identical content must hash equal")
	}
	if contentHash("we use postgres") == contentHash("we use mysql") {
		t.Error("distinct content must hash differently")
	}
}

// nearDupMockStore holds one stored memory and answers the normalized-hash
// lookup the way the SQLite store does, so the assertions below exercise the
// predicate rather than a mocked verdict.
type nearDupMockStore struct {
	Store
	stored Memory
}

func (m *nearDupMockStore) FindByNormalizedHash(_ context.Context, hash string, _ *string) (*Memory, error) {
	if normalizedHash(m.stored.Content) != hash {
		return nil, nil
	}
	found := m.stored
	return &found, nil
}

func TestFindNearDuplicate(t *testing.T) {
	svc := &Service{logger: slog.Default()}
	content := "The sync engine uses watermark and tombstones for delta updates"

	// Same text modulo case/whitespace IS a duplicate.
	svc.store = &nearDupMockStore{stored: Memory{
		ID: "x", Content: "the   sync engine uses watermark and tombstones for delta updates\n",
	}}
	if dup := svc.findNearDuplicate(context.Background(), content, nil); dup == nil || dup.ID != "x" {
		t.Fatalf("expected case/whitespace variant match 'x', got %v", dup)
	}

	// A near-identical-but-DIFFERENT restatement must NOT merge (only one extra
	// word). This is the over-merge the old Jaccard approach caused.
	svc.store = &nearDupMockStore{stored: Memory{
		ID: "y", Content: "the sync engine uses watermark and tombstones for delta updates always",
	}}
	if dup := svc.findNearDuplicate(context.Background(), content, nil); dup != nil {
		t.Fatalf("near-identical but distinct content must NOT merge, got %v", dup)
	}

	// A genuinely different fact must NOT merge.
	svc.store = &nearDupMockStore{stored: Memory{
		ID: "z", Content: "the billing service charges invoices monthly via stripe",
	}}
	if dup := svc.findNearDuplicate(context.Background(), content, nil); dup != nil {
		t.Fatalf("expected no near-dup, got %v", dup)
	}
}

// TestFindNearDuplicate_StoreErrorFailsOpen keeps the save path inside its
// contract: a lookup failure must let the save proceed as a new memory, never
// abort it or merge into something arbitrary.
func TestFindNearDuplicate_StoreErrorFailsOpen(t *testing.T) {
	svc := &Service{logger: slog.Default(), store: &nearDupErrStore{}}
	if dup := svc.findNearDuplicate(context.Background(), "anything at all", nil); dup != nil {
		t.Fatalf("a failed lookup must not produce a duplicate, got %v", dup)
	}
}

type nearDupErrStore struct{ Store }

func (m *nearDupErrStore) FindByNormalizedHash(_ context.Context, _ string, _ *string) (*Memory, error) {
	return nil, errors.New("database is locked")
}
