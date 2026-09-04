package memory

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestLedgerIdentityGuardRejectsOrphanInserts pins the invariant migration 021
// had to repair after the fact: a memory row without ledger identity is
// invisible to curation (which resolves through current_revision_id) and to the
// embedding queue (which joins on it), so it is a half-write that only surfaces
// later as a nightly error and a missing embedding.
func TestLedgerIdentityGuardRejectsOrphanInserts(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "guard.db"), nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	db := store.DB()

	cases := []struct {
		name    string
		columns string
		values  string
	}{
		{"no ledger columns at all", "", ""},
		{"empty logical_id", ", logical_id, current_revision_id", ", '', 'rev-1'"},
		{"empty current_revision_id", ", logical_id, current_revision_id", ", 'm-1', ''"},
		{"null logical_id", ", logical_id, current_revision_id", ", NULL, 'rev-1'"},
		{"null current_revision_id", ", logical_id, current_revision_id", ", 'm-1', NULL"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(
				`INSERT INTO memories (id, category, content, content_hash, keywords, source` +
					tc.columns + `) VALUES ('m-1', 'fact', 'conteudo', 'h', '[]', 'test'` + tc.values + `)`)
			if err == nil {
				t.Fatal("insert without ledger identity was accepted; the guard is not in force")
			}
			if !strings.Contains(err.Error(), "requires logical_id and current_revision_id") {
				t.Errorf("unexpected error: %v", err)
			}

			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = 'm-1'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("row was written anyway: count = %d", n)
			}
		})
	}
}

// TestLedgerIdentityGuardAllowsCompleteInserts makes sure the guard does not
// stand in the way of a well-formed write.
func TestLedgerIdentityGuardAllowsCompleteInserts(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "guard-ok.db"), nil)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	if _, err := store.DB().Exec(
		`INSERT INTO memories (id, category, content, content_hash, keywords, source,
		                       logical_id, current_revision_id)
		 VALUES ('m-2', 'fact', 'conteudo', 'h', '[]', 'test', 'm-2', 'rev-2')`,
	); err != nil {
		t.Fatalf("well-formed insert rejected: %v", err)
	}
}
