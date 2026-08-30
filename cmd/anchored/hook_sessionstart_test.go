package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/contextbudget"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/session"

	_ "github.com/mattn/go-sqlite3"

	"github.com/jholhewres/anchored/pkg/config"
)

// newSessionStartTestDB runs the REAL client migrations so the queries in the
// sessionstart hook are validated against the production schema (pinned and
// curation_status live in the metadata JSON column, not as real columns — a
// hand-rolled schema here would hide that class of bug).
func newSessionStartTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := memory.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func insertSessionStartMem(t *testing.T, db *sql.DB, id, projectID, category, content, metadata string, deleted bool) {
	t.Helper()
	deletedAt := any(nil)
	if deleted {
		deletedAt = "2026-01-01T00:00:00Z"
	}
	_, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, content_hash, created_at, updated_at, metadata, deleted_at)
		 VALUES (?, ?, ?, ?, '', datetime('now'), datetime('now'), ?, ?)`,
		id, projectID, category, content, metadata, deletedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// TestQueryDecisions_MetadataPinnedAndLifecycle is the regression test for the
// schema mismatch: pinned/curation_status are metadata JSON fields in the
// client. It also guards the soft-delete and low-signal exclusions.
func TestQueryDecisions_MetadataPinnedAndLifecycle(t *testing.T) {
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}

	insertSessionStartMem(t, db, "m1", "proj1", "fact",
		"pinned architectural ground rule", `{"pinned": true}`, false)
	insertSessionStartMem(t, db, "m2", "proj1", "decision",
		"we settled on postgres for the server", `{}`, false)
	insertSessionStartMem(t, db, "m3", "proj1", "decision",
		"deleted decision must not appear", `{}`, true)
	insertSessionStartMem(t, db, "m4", "proj1", "learning",
		"low signal noise should be excluded", `{"curation_status": "low_signal"}`, false)
	insertSessionStartMem(t, db, "m5", "proj1", "fact",
		"plain fact without pin is not a decision", `{}`, false)

	items := queryDecisions(context.Background(), hc, "proj1")
	if len(items) != 2 {
		t.Fatalf("want 2 items (pinned + decision), got %d: %+v", len(items), items)
	}
	// Pinned memory must come first (priority 0).
	if !strings.Contains(items[0].Text, "pinned architectural ground rule") {
		t.Errorf("pinned memory should rank first, got: %s", items[0].Text)
	}
	if items[0].Priority != 0 {
		t.Errorf("pinned item priority = %d, want 0", items[0].Priority)
	}
	if !strings.Contains(items[1].Text, "settled on postgres") {
		t.Errorf("decision memory missing, got: %s", items[1].Text)
	}
	for _, it := range items {
		if strings.Contains(it.Text, "deleted decision") {
			t.Error("soft-deleted memory leaked into the sessionstart block")
		}
		if strings.Contains(it.Text, "low signal noise") {
			t.Error("low_signal memory leaked into the sessionstart block")
		}
	}
}

// TestBuildSessionStartTiers_FailSafeOnEmptyDB ensures an empty (but migrated)
// DB produces tiers without errors and Assemble-able output.
func TestBuildSessionStartTiers_FailSafeOnEmptyDB(t *testing.T) {
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}

	tiers := buildSessionStartTiers(context.Background(), hc, "", "proj-none", "")
	if len(tiers) != 5 {
		t.Fatalf("want 5 tiers (standing_rules first), got %d", len(tiers))
	}
	// standing_rules/decisions/task/events must be empty on a fresh DB
	// (identity may pick up the developer's real ~/.anchored/identity.md,
	// which is fine).
	for _, tier := range tiers {
		switch tier.Name {
		case "standing_rules", "task", "events", "decisions":
			if len(tier.Items) != 0 {
				t.Errorf("tier %s should be empty on fresh DB, got %d items", tier.Name, len(tier.Items))
			}
		}
	}
}

// TestQueryDirectives_ScopingAndOrder guards Feature A: global directives load
// in every project, project-scoped ones only in theirs, soft-deleted and
// non-directive rows are excluded, and order is stable (oldest first).
func TestQueryDirectives_ScopingAndOrder(t *testing.T) {
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}

	insertSessionStartMem(t, db, "d1", "", "preference",
		"never commit without an explicit request", `{"directive": true, "pinned": true}`, false)
	insertSessionStartMem(t, db, "d2", "proj1", "preference",
		"in this repo always run make lint before pushing", `{"directive": true}`, false)
	insertSessionStartMem(t, db, "d3", "proj2", "preference",
		"other project rule must not leak", `{"directive": true}`, false)
	insertSessionStartMem(t, db, "d4", "", "preference",
		"deleted rule must not appear", `{"directive": true}`, true)
	insertSessionStartMem(t, db, "d5", "", "preference",
		"a plain preference is not a directive", `{}`, false)

	items := queryDirectives(context.Background(), hc, "proj1")
	if len(items) != 2 {
		t.Fatalf("want 2 directives (global + proj1), got %d: %+v", len(items), items)
	}
	if !strings.Contains(items[0].Text, "never commit") || !strings.Contains(items[0].Text, `scope="user"`) {
		t.Errorf("first directive should be the global one with scope=user, got: %s", items[0].Text)
	}
	if !strings.Contains(items[1].Text, "make lint") || !strings.Contains(items[1].Text, `scope="project"`) {
		t.Errorf("second directive should be project-scoped, got: %s", items[1].Text)
	}
	for _, it := range items {
		if strings.Contains(it.Text, "must not leak") || strings.Contains(it.Text, "deleted rule") || strings.Contains(it.Text, "plain preference") {
			t.Errorf("leaked excluded row: %s", it.Text)
		}
	}
}

// TestBuildSessionStartTiers_StandingRulesFirst ensures the directives tier is
// tier 0 and survives the budget even with large lower tiers (MinItems).
func TestBuildSessionStartTiers_StandingRulesFirst(t *testing.T) {
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}

	insertSessionStartMem(t, db, "d1", "", "preference",
		"never push directly to main", `{"directive": true}`, false)
	big := strings.Repeat("a long decision body ", 200)
	for i := 0; i < 5; i++ {
		insertSessionStartMem(t, db, fmt.Sprintf("m%d", i), "proj1", "decision", big, `{}`, false)
	}

	tiers := buildSessionStartTiers(context.Background(), hc, "", "proj1", "")
	if tiers[0].Name != "standing_rules" {
		t.Fatalf("tier 0 = %s, want standing_rules", tiers[0].Name)
	}
	out, _ := contextbudget.Assemble(tiers, 2000)
	if !strings.Contains(out, "never push directly to main") {
		t.Error("standing rule must survive the budget ahead of large decisions")
	}
}

// TestQueryDecisions_ExcludesDirectives guards the double-injection bug: a
// pinned directive must appear only in the standing_rules tier, never also in
// the decisions tier.
func TestQueryDecisions_ExcludesDirectives(t *testing.T) {
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}

	insertSessionStartMem(t, db, "d1", "proj1", "preference",
		"never push directly to main", `{"directive": true, "pinned": true}`, false)
	insertSessionStartMem(t, db, "m1", "proj1", "decision",
		"we settled on postgres", `{}`, false)

	items := queryDecisions(context.Background(), hc, "proj1")
	for _, it := range items {
		if strings.Contains(it.Text, "never push directly") {
			t.Fatalf("directive leaked into decisions tier: %s", it.Text)
		}
	}
	if len(items) != 1 {
		t.Errorf("want 1 decision, got %d", len(items))
	}
}

// TestTaskThreadItem_CrossRepoInjection locks Feature B's core promise: when
// the branch carries a ticket key, the sessionstart tier shows the thread AND
// what the same task touched in OTHER projects (name + files), as references.
func TestTaskThreadItem_CrossRepoInjection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	db := newSessionStartTestDB(t)
	hc := &HookContext{db: db}
	mgr := session.NewManager(db, nil)
	ctx := context.Background()

	// Repo on a ticket branch.
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "checkout", "-q", "-b", "feature/PROJ-77-cross-repo"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	// The same task already touched repoB (another project) in session sB.
	if _, err := db.Exec(`INSERT INTO projects (id, name, path) VALUES ('projB', 'repo-b', '/tmp/repo-b')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO working_sets (session_id, project_id, files, updated_at)
		VALUES ('sB', 'projB', '["api/handler.go","api/router.go"]', datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.UpsertTaskThread(ctx, "PROJ-77", session.TaskThreadDelta{ProjectID: "projB", SessionID: "sB", JournalNote: "decided the wire format"}); err != nil {
		t.Fatal(err)
	}

	item, ok := taskThreadItem(ctx, hc, mgr, "sA", "projA", repo)
	if !ok {
		t.Fatal("taskThreadItem should fire on a ticket branch")
	}
	for _, want := range []string{
		`key="PROJ-77"`,
		`project="repo-b"`,
		"api/handler.go",
		"decided the wire format",
	} {
		if !strings.Contains(item.Text, want) {
			t.Errorf("cross-repo block missing %q\n--- block ---\n%s", want, item.Text)
		}
	}

	// The session/project were registered on the thread.
	th, _ := mgr.GetTaskThread(ctx, "PROJ-77")
	if len(th.ProjectIDs) != 2 || len(th.SessionIDs) != 2 {
		t.Fatalf("session not registered on thread: %+v", th)
	}

	// Non-ticket branch: no item.
	repo2 := t.TempDir()
	exec.Command("git", "init", "-q", repo2).Run()
	if _, ok := taskThreadItem(ctx, hc, mgr, "sC", "projA", repo2); ok {
		t.Error("non-ticket branch must not produce a task item")
	}
}

// TestAppendRichContextBlock_MarksWhatItEmits welds the two halves together:
// the rich block reaching the model and the marker recording that it did. They
// must not drift apart — the marker is the only signal the UserPromptSubmit
// recall has for telling "memory was already delivered this session" from "the
// model has seen nothing yet".
func TestAppendRichContextBlock_MarksWhatItEmits(t *testing.T) {
	// appendRichContextBlock reaches the real ~/.anchored/identity.md through
	// richBlockCarriesIdentity, so without an isolated HOME this passes only
	// on machines that have no identity file — CI and nowhere else. Every
	// developer with anchored actually installed would see it red.
	t.Setenv("HOME", t.TempDir())

	cfgFor := func(dir, mode string) *config.Config {
		c := &config.Config{}
		c.Memory.StorageDir = dir
		c.Plugin.ContextGate = mode
		return c
	}
	markerFor := func(dir, sess string) string {
		return richMarkerPath(filepath.Join(dir, "ctxgate"), sess)
	}

	t.Run("empty block emits nothing and marks nothing", func(t *testing.T) {
		dir := t.TempDir()
		got := appendRichContextBlock("base", "", cfgFor(dir, "enforce"), "s1")
		if got != "base" {
			t.Errorf("additional = %q, want unchanged", got)
		}
		if _, err := os.Stat(markerFor(dir, "s1")); !os.IsNotExist(err) {
			t.Error("marked the session although no rich block was emitted")
		}
	})

	t.Run("non-empty block is emitted and marked", func(t *testing.T) {
		dir := t.TempDir()
		got := appendRichContextBlock("base", "IDENTITY", cfgFor(dir, "enforce"), "s2")
		if !strings.Contains(got, "<anchored_context>") || !strings.Contains(got, "IDENTITY") {
			t.Errorf("rich block not emitted: %q", got)
		}
		if _, err := os.Stat(markerFor(dir, "s2")); err != nil {
			t.Errorf("emitted the rich block without marking the session: %v", err)
		}
	})

	t.Run("gate disabled still emits, does not mark", func(t *testing.T) {
		dir := t.TempDir()
		got := appendRichContextBlock("base", "IDENTITY", cfgFor(dir, "disabled"), "s3")
		if !strings.Contains(got, "<anchored_context>") {
			t.Error("rich block must be emitted regardless of gate mode")
		}
		if _, err := os.Stat(markerFor(dir, "s3")); !os.IsNotExist(err) {
			t.Error("marked the session while the gate is disabled")
		}
	})

	t.Run("nil config still emits", func(t *testing.T) {
		if got := appendRichContextBlock("base", "IDENTITY", nil, "s4"); !strings.Contains(got, "IDENTITY") {
			t.Error("nil config must not suppress the rich block")
		}
	})
}

// TestRichBlockCarriesIdentity pins the premise the recall credit rests on.
// contextbudget.Assemble does not guarantee MinItems, so a non-empty rich
// block is NOT evidence that identity reached the model — long standing
// directives in tier 0 can push it out. Marking on non-emptiness would credit
// the gate on a signal that never arrived.
func TestRichBlockCarriesIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// No identity file: nothing to deliver, so any block qualifies.
	if !richBlockCarriesIdentity("decisions only") {
		t.Error("absent identity.md must not disqualify a session")
	}

	const identity = "Aron prefers small, thematic commits."
	if err := os.MkdirAll(filepath.Join(home, ".anchored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".anchored", "identity.md"), []byte(identity), 0o644); err != nil {
		t.Fatal(err)
	}

	if !richBlockCarriesIdentity("## identity\n" + identity + "\n## decisions\n…") {
		t.Error("block containing the identity text should qualify")
	}
	// The load-bearing case: budget consumed by tier 0, identity dropped.
	if richBlockCarriesIdentity("## standing rules\n- rule\n- rule\n## decisions\n…") {
		t.Error("block without the identity text must NOT be marked as carrying L0")
	}
	if richBlockCarriesIdentity("") {
		t.Error("empty block cannot carry identity")
	}
}
