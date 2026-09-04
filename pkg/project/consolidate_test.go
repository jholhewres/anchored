package project

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// openConsolidateTestDB adds the project-scoped tables the consolidation walks.
// project_baseline is included on purpose: project_id is its primary key, so it
// is the table that proves the collision path — the destination keeps its own
// row and the worktree's derived copy is dropped rather than colliding.
func openConsolidateTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openTestDB(t)
	if _, err := db.Exec(`
		CREATE TABLE memories (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			category TEXT NOT NULL,
			content TEXT NOT NULL,
			content_hash TEXT,
			keywords TEXT,
			source TEXT,
			logical_id TEXT,
			current_revision_id TEXT
		);
		CREATE TABLE project_baseline (
			project_id TEXT PRIMARY KEY,
			summary TEXT
		);
	`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

// seedFragmentedProject reproduces what the old detector wrote: a project row
// of the worktree's own path, holding memories the main checkout cannot see.
func seedFragmentedProject(t *testing.T, db *sql.DB, id, name, path string, memories int) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO projects (id, name, path) VALUES (?, ?, ?)`, id, name, path,
	); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
	for i := 0; i < memories; i++ {
		mid := id + "-mem-" + string(rune('a'+i))
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, content_hash, keywords,
			                       source, logical_id, current_revision_id)
			 VALUES (?, ?, 'fact', 'conteudo', ?, '[]', 'test', ?1, ?1)`,
			mid, id, "hash-"+mid,
		); err != nil {
			t.Fatalf("seed memory: %v", err)
		}
	}
}

func countMemories(t *testing.T, db *sql.DB, projectID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "t@example.com")
	runGit(t, dir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "f.txt")
	runGit(t, dir, "commit", "-qm", "init")
}

// TestConsolidate_FoldsWorktreeIntoRepository is the repair path: a worktree
// that accumulated its own memories is folded into the repository's project,
// and the worktree's project row goes away.
func TestConsolidate_FoldsWorktreeIntoRepository(t *testing.T) {
	db := openConsolidateTestDB(t)
	d := NewDetector(db)

	base := t.TempDir()
	main := filepath.Join(base, "repo")
	initRepo(t, main)
	wt := filepath.Join(base, "wt")
	runGit(t, main, "worktree", "add", "-q", "--detach", wt)

	seedFragmentedProject(t, db, "proj-main", "repo", main, 2)
	seedFragmentedProject(t, db, "proj-wt", "wt", wt, 3)

	report, err := d.PlanConsolidate()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(report.Merges) != 1 {
		t.Fatalf("merges = %d, want 1: %+v", len(report.Merges), report.Merges)
	}
	m := report.Merges[0]
	if m.Kind != KindMerge {
		t.Errorf("kind = %q, want %q", m.Kind, KindMerge)
	}
	if m.From.ID != "proj-wt" || m.Into.ID != "proj-main" {
		t.Errorf("merge %s -> %s, want proj-wt -> proj-main", m.From.ID, m.Into.ID)
	}
	if got := m.Rows["memories"]; got != 3 {
		t.Errorf("memories to move = %d, want 3", got)
	}

	// A plan is only a plan.
	if countMemories(t, db, "proj-wt") != 3 {
		t.Error("dry run moved rows")
	}

	if err := d.ApplyConsolidate(report); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := countMemories(t, db, "proj-main"); got != 5 {
		t.Errorf("main project memories = %d, want 5", got)
	}
	if got := countMemories(t, db, "proj-wt"); got != 0 {
		t.Errorf("worktree memories left behind = %d", got)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE id = 'proj-wt'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Error("worktree project row survived the merge")
	}
}

// TestConsolidate_RepathsWhenRepositoryHasNoRow covers the case where only the
// worktree was ever seen: there is nothing to merge into, so the row moves onto
// the repository's path instead of being dropped.
func TestConsolidate_RepathsWhenRepositoryHasNoRow(t *testing.T) {
	db := openConsolidateTestDB(t)
	d := NewDetector(db)

	base := t.TempDir()
	main := filepath.Join(base, "repo")
	initRepo(t, main)
	wt := filepath.Join(base, "wt")
	runGit(t, main, "worktree", "add", "-q", "--detach", wt)

	seedFragmentedProject(t, db, "proj-wt", "wt", wt, 2)

	report, err := d.PlanConsolidate()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(report.Merges) != 1 || report.Merges[0].Kind != KindRepath {
		t.Fatalf("want one repath, got %+v", report.Merges)
	}
	if err := d.ApplyConsolidate(report); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var path string
	if err := db.QueryRow(`SELECT path FROM projects WHERE id = 'proj-wt'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(path) != filepath.Clean(main) {
		t.Errorf("path = %q, want %q", path, main)
	}
	if got := countMemories(t, db, "proj-wt"); got != 2 {
		t.Errorf("memories = %d, want 2 — a repath must not lose rows", got)
	}
}

// TestConsolidate_LeavesUnrelatedProjectsAlone guards everything the command
// must not touch: independent clones, submodules, and plain checkouts.
func TestConsolidate_LeavesUnrelatedProjectsAlone(t *testing.T) {
	db := openConsolidateTestDB(t)
	d := NewDetector(db)

	base := t.TempDir()
	cloneA := filepath.Join(base, "clone-a")
	cloneB := filepath.Join(base, "clone-b")
	initRepo(t, cloneA)
	initRepo(t, cloneB)

	sub := filepath.Join(base, "sub")
	super := filepath.Join(base, "super")
	initRepo(t, sub)
	initRepo(t, super)
	runGit(t, super, "-c", "protocol.file.allow=always",
		"submodule", "add", "-q", sub, "vendor/sub")
	runGit(t, super, "commit", "-qm", "add submodule")

	seedFragmentedProject(t, db, "p-a", "clone-a", cloneA, 1)
	seedFragmentedProject(t, db, "p-b", "clone-b", cloneB, 1)
	seedFragmentedProject(t, db, "p-super", "super", super, 1)
	seedFragmentedProject(t, db, "p-sub", "sub", filepath.Join(super, "vendor", "sub"), 1)

	report, err := d.PlanConsolidate()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(report.Merges) != 0 {
		t.Errorf("nothing should be folded, got %+v", report.Merges)
	}
	if report.Scanned != 4 {
		t.Errorf("scanned = %d, want 4", report.Scanned)
	}
}

// TestConsolidate_ReportsMissingPathsWithoutGuessing covers a project whose
// directory is gone: it cannot be proven to be a worktree, so it is reported
// and left exactly as it is.
func TestConsolidate_ReportsMissingPathsWithoutGuessing(t *testing.T) {
	db := openConsolidateTestDB(t)
	d := NewDetector(db)

	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	seedFragmentedProject(t, db, "p-gone", "gone", gone, 4)

	report, err := d.PlanConsolidate()
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(report.Merges) != 0 {
		t.Errorf("a missing path must not be folded, got %+v", report.Merges)
	}
	if len(report.Missing) != 1 || report.Missing[0].Project.ID != "p-gone" {
		t.Fatalf("missing = %+v, want p-gone", report.Missing)
	}
	if got := report.Missing[0].Memories; got != 4 {
		t.Errorf("stranded memories = %d, want 4 — the report must say what is sitting there", got)
	}
	if got := countMemories(t, db, "p-gone"); got != 4 {
		t.Errorf("memories = %d, want 4 untouched", got)
	}
}

// TestConsolidate_IsIdempotent: running it twice must be a no-op the second
// time, not a second round of moves.
func TestConsolidate_IsIdempotent(t *testing.T) {
	db := openConsolidateTestDB(t)
	d := NewDetector(db)

	base := t.TempDir()
	main := filepath.Join(base, "repo")
	initRepo(t, main)
	wt := filepath.Join(base, "wt")
	runGit(t, main, "worktree", "add", "-q", "--detach", wt)

	seedFragmentedProject(t, db, "proj-main", "repo", main, 1)
	seedFragmentedProject(t, db, "proj-wt", "wt", wt, 2)

	first, err := d.PlanConsolidate()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyConsolidate(first); err != nil {
		t.Fatal(err)
	}

	second, err := d.PlanConsolidate()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Merges) != 0 {
		t.Errorf("second run still wants to fold %+v", second.Merges)
	}
	if got := countMemories(t, db, "proj-main"); got != 3 {
		t.Errorf("memories = %d, want 3", got)
	}
}
