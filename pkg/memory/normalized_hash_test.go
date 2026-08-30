package memory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func newNormalizedHashStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := NewSQLiteStore(filepath.Join(t.TempDir(), "s.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// The near-duplicate check has always accepted a candidate only when its
// content is identical after lowercasing and collapsing whitespace. It used to
// find that candidate with an FTS5 query built from every keyword in the
// incoming content, which costs seconds per save on a large store. These tests
// pin the predicate itself, so the indexed implementation stays honest about
// what the slow one promised.

func TestNormalizedHash_IgnoresCaseAndWhitespace(t *testing.T) {
	base := normalizedHash("Prefer small thematic commits")

	same := []string{
		"prefer small thematic commits",
		"PREFER SMALL THEMATIC COMMITS",
		"  Prefer   small\tthematic\ncommits  ",
	}
	for _, s := range same {
		if got := normalizedHash(s); got != base {
			t.Errorf("normalizedHash(%q) = %s, want %s", s, got, base)
		}
	}

	if normalizedHash("prefer small thematic commit") == base {
		t.Error("a different wording must not collide")
	}
	if normalizedHash("") == base {
		t.Error("empty content must not collide with real content")
	}
}

// TestNormalizedHash_MatchesLegacyPredicate keeps the new key aligned with the
// comparison the FTS path performed on its candidates. If normalizeForDedup
// ever changes, both must move together or dedup silently changes meaning.
func TestNormalizedHash_MatchesLegacyPredicate(t *testing.T) {
	pairs := [][2]string{
		{"Deploy on Friday", "deploy   on friday"},
		{"a\tb", "a b"},
		{"Ünïcode Ãccents", "ünïcode ãccents"},
	}
	for _, p := range pairs {
		legacyEqual := normalizeForDedup(p[0]) == normalizeForDedup(p[1])
		hashEqual := normalizedHash(p[0]) == normalizedHash(p[1])
		if legacyEqual != hashEqual {
			t.Errorf("%q vs %q: legacy=%v hash=%v", p[0], p[1], legacyEqual, hashEqual)
		}
	}
}

func TestFindByNormalizedHash_RoundTrip(t *testing.T) {
	store := newNormalizedHashStore(t)
	ctx := context.Background()

	m := Memory{
		ID:          newUUID(),
		Content:     "Standing rule: never force-push main",
		Category:    "preference",
		Source:      "test",
		ContentHash: contentHash("Standing rule: never force-push main"),
	}
	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A restatement differing only in case and spacing must resolve to the
	// stored row — this is the whole point of the column.
	found, err := store.FindByNormalizedHash(ctx, normalizedHash("STANDING RULE:   never force-push  main"), nil)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found == nil {
		t.Fatal("normalized restatement did not find the stored memory")
	}
	if found.ID != m.ID {
		t.Errorf("found %s, want %s", found.ID, m.ID)
	}

	missing, err := store.FindByNormalizedHash(ctx, normalizedHash("something else entirely"), nil)
	if err != nil {
		t.Fatalf("lookup miss: %v", err)
	}
	if missing != nil {
		t.Errorf("unrelated content matched %s", missing.ID)
	}

	if empty, err := store.FindByNormalizedHash(ctx, "", nil); err != nil || empty != nil {
		t.Errorf("empty hash must be a clean miss, got (%v, %v)", empty, err)
	}
}

// TestFindByNormalizedHash_ScopedByProject mirrors FindByContentHash: dedup is
// per-project, so the same sentence in two projects is two memories.
func TestFindByNormalizedHash_ScopedByProject(t *testing.T) {
	store := newNormalizedHashStore(t)
	ctx := context.Background()

	projA := "project-a"
	content := "Run the migration before deploying"
	m := Memory{
		ID:          newUUID(),
		Content:     content,
		Category:    "fact",
		Source:      "test",
		ProjectID:   &projA,
		ContentHash: contentHash(content),
	}
	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("save: %v", err)
	}

	hash := normalizedHash(content)

	found, err := store.FindByNormalizedHash(ctx, hash, &projA)
	if err != nil || found == nil {
		t.Fatalf("same project should match: (%v, %v)", found, err)
	}

	projB := "project-b"
	other, err := store.FindByNormalizedHash(ctx, hash, &projB)
	if err != nil {
		t.Fatalf("other project lookup: %v", err)
	}
	if other != nil {
		t.Error("a memory from another project must not be treated as a duplicate")
	}

	global, err := store.FindByNormalizedHash(ctx, hash, nil)
	if err != nil {
		t.Fatalf("global lookup: %v", err)
	}
	if global != nil {
		t.Error("a project-scoped memory must not answer an unscoped lookup")
	}
}

// TestBackfillNormalizedHash_StampsLegacyRows covers the upgrade path: rows
// written before the column exists hold NULL and are invisible to dedup until
// stamped.
func TestBackfillNormalizedHash_StampsLegacyRows(t *testing.T) {
	store := newNormalizedHashStore(t)
	ctx := context.Background()

	const content = "Legacy row written before the column existed"
	m := Memory{
		ID:          newUUID(),
		Content:     content,
		Category:    "fact",
		Source:      "test",
		ContentHash: contentHash(content),
	}
	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Simulate the pre-migration state.
	if _, err := store.db.ExecContext(ctx,
		"UPDATE memories SET normalized_hash = NULL WHERE id = ?", m.ID); err != nil {
		t.Fatal(err)
	}

	pending, err := store.PendingNormalizedHash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}

	if found, _ := store.FindByNormalizedHash(ctx, normalizedHash(content), nil); found != nil {
		t.Fatal("an unstamped row must not be findable — the test premise is wrong")
	}

	stamped, err := store.BackfillNormalizedHash(ctx, 100)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if stamped != 1 {
		t.Fatalf("stamped = %d, want 1", stamped)
	}

	found, err := store.FindByNormalizedHash(ctx, normalizedHash(content), nil)
	if err != nil || found == nil {
		t.Fatalf("stamped row should be findable: (%v, %v)", found, err)
	}

	if pending, err := store.PendingNormalizedHash(ctx); err != nil || pending != 0 {
		t.Errorf("pending after backfill = %d (err %v), want 0", pending, err)
	}

	// Idempotent: a second pass has nothing to do.
	if again, err := store.BackfillNormalizedHash(ctx, 100); err != nil || again != 0 {
		t.Errorf("second backfill stamped %d (err %v), want 0", again, err)
	}
}

// TestBackfillNormalizedHash_RespectsLimit keeps a large store draining in
// slices rather than one long transaction.
func TestBackfillNormalizedHash_RespectsLimit(t *testing.T) {
	store := newNormalizedHashStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		content := "legacy memory number " + string(rune('a'+i))
		m := Memory{
			ID:          newUUID(),
			Content:     content,
			Category:    "fact",
			Source:      "test",
			ContentHash: contentHash(content),
		}
		if err := store.Save(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE memories SET normalized_hash = NULL"); err != nil {
		t.Fatal(err)
	}

	stamped, err := store.BackfillNormalizedHash(ctx, 2)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if stamped != 2 {
		t.Fatalf("stamped = %d, want the limit of 2", stamped)
	}
	if pending, _ := store.PendingNormalizedHash(ctx); pending != 3 {
		t.Errorf("pending = %d, want 3", pending)
	}
}

// TestSaveStampsNormalizedHash pins that the ordinary write path maintains the
// column, so a fresh install never needs the backfill at all.
func TestSaveStampsNormalizedHash(t *testing.T) {
	store := newNormalizedHashStore(t)
	ctx := context.Background()

	const content = "Fresh write should be stamped"
	m := Memory{
		ID:          newUUID(),
		Content:     content,
		Category:    "fact",
		Source:      "test",
		ContentHash: contentHash(content),
	}
	if err := store.Save(ctx, m); err != nil {
		t.Fatal(err)
	}

	if pending, err := store.PendingNormalizedHash(ctx); err != nil || pending != 0 {
		t.Fatalf("a fresh save left %d rows unstamped (err %v)", pending, err)
	}

	// And an update through the same path re-stamps rather than going stale.
	m.Content = "Rewritten content entirely"
	m.ContentHash = contentHash(m.Content)
	if err := store.Save(ctx, m); err != nil {
		t.Fatal(err)
	}
	found, err := store.FindByNormalizedHash(ctx, normalizedHash(m.Content), nil)
	if err != nil || found == nil {
		t.Fatalf("update did not re-stamp: (%v, %v)", found, err)
	}
	if stale, _ := store.FindByNormalizedHash(ctx, normalizedHash(content), nil); stale != nil {
		t.Error("the pre-update hash still resolves — the stamp went stale")
	}
}
