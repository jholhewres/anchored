package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

func TestFindDreamLost_SelectsOnlyDreamDeletesWithoutALiveCopy(t *testing.T) {
	ctx := context.Background()
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "restore.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	db := store.DB()

	p1, p2 := "proj-1", "proj-2"
	save := func(id, content, source string, project *string) {
		t.Helper()
		if _, err := store.SaveTemporal(ctx, memory.Memory{
			ID: id, Category: "fact", Content: content, Source: source, ProjectID: project,
		}, memory.TemporalWriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	rawDelete := func(id string) {
		t.Helper()
		if _, err := db.Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	action := func(id, memoryID, relatedID, kind string) {
		t.Helper()
		// A completed run whose consolidation window covers the deletes below:
		// dream records the run just before it applies the proposals.
		if _, err := db.Exec("INSERT OR IGNORE INTO dream_runs (id, started_at, finished_at, status) VALUES ('run', ?, ?, 'completed')",
			time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(
			"INSERT INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, status) VALUES (?, 'run', ?, ?, ?, 1.0, 'test', 'proposed')",
			id, memoryID, relatedID, kind); err != nil {
			t.Fatal(err)
		}
	}

	// Lost: dream deleted the only copy in its project.
	save("lost", "only copy", "bootstrap", &p1)
	rawDelete("lost")
	action("a-lost", "lost", "elsewhere", "dedup")

	// Lost too: the surviving twin lives in another project.
	save("lost-cross", "shared text", "mcp", &p1)
	save("twin-p2", "shared text", "mcp", &p2)
	rawDelete("lost-cross")
	action("a-cross", "lost-cross", "twin-p2", "dedup")

	// Lost: absorbed by a merge, which deletes the related memory.
	save("absorbed", "absorbed text", "mcp", &p1)
	rawDelete("absorbed")
	action("a-merge", "keeper-gone", "absorbed", "merge")

	// Not lost: a live copy remains in the same project.
	save("dup", "kept text", "mcp", &p1)
	save("keeper", "kept text", "mcp", &p1)
	rawDelete("dup")
	action("a-dup", "dup", "keeper", "dedup")

	// Not lost: deleted on purpose through the ledger.
	save("forgotten", "forgotten text", "mcp", &p1)
	if err := store.SoftDelete(ctx, "forgotten"); err != nil {
		t.Fatal(err)
	}
	action("a-forgot", "forgotten", "x", "dedup")

	// Not dream's doing: raw-deleted but never proposed.
	save("other", "other text", "mcp", &p1)
	rawDelete("other")

	// Transcript import: only with includeImported.
	save("imported", "narration line", "claude-code", &p1)
	rawDelete("imported")
	action("a-imp", "imported", "x", "dedup")

	ids := func(includeImported bool) []string {
		t.Helper()
		lost, err := findDreamLost(ctx, db, includeImported)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range lost {
			out = append(out, m.ID)
		}
		sort.Strings(out)
		return out
	}

	if got, want := ids(false), []string{"absorbed", "lost", "lost-cross"}; !equalStrings(got, want) {
		t.Errorf("default selection: got %v, want %v", got, want)
	}
	if got, want := ids(true), []string{"absorbed", "imported", "lost", "lost-cross"}; !equalStrings(got, want) {
		t.Errorf("with imports: got %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
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

// dreamLostFixture is a store plus the writes the dream-lost tests share.
type dreamLostFixture struct {
	t     *testing.T
	store *memory.SQLiteStore
	db    *sql.DB
}

func newDreamLostFixture(t *testing.T) *dreamLostFixture {
	t.Helper()
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "restore.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &dreamLostFixture{t: t, store: store, db: store.DB()}
}

func (f *dreamLostFixture) run(id, status string, started, finished time.Time) {
	f.t.Helper()
	if _, err := f.db.Exec("INSERT INTO dream_runs (id, started_at, finished_at, status) VALUES (?, ?, ?, ?)",
		id, started, finished, status); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dreamLostFixture) save(id, content string) {
	f.t.Helper()
	p := "proj-1"
	if _, err := f.store.SaveTemporal(context.Background(), memory.Memory{
		ID: id, Category: "fact", Content: content, Source: "bootstrap", ProjectID: &p,
	}, memory.TemporalWriteOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dreamLostFixture) proposeAndRawDelete(id, runID string) {
	f.t.Helper()
	if _, err := f.db.Exec(
		"INSERT INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, status) VALUES (?, ?, ?, 'x', 'dedup', 1.0, 'test', 'proposed')",
		"a-"+id, runID, id); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?", id); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dreamLostFixture) lostIDs() []string {
	f.t.Helper()
	lost, err := findDreamLost(context.Background(), f.db, true)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, m := range lost {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// purge, curation clean and directives rm also delete with a raw UPDATE, and
// dream proposals cover most of the corpus; only a delete that happened while
// a dream run was applying its proposals is dream's doing.
func TestFindDreamLost_IgnoresDeletesOutsideACompletedRun(t *testing.T) {
	f := newDreamLostFixture(t)
	now := time.Now()
	f.run("dry", "dry_run", now.Add(-2*time.Minute), now.Add(-time.Minute))
	f.run("old", "completed", now.Add(-48*time.Hour), now.Add(-47*time.Hour))

	f.save("purged", "deleted on purpose later")
	f.proposeAndRawDelete("purged", "dry")

	if got := f.lostIDs(); len(got) != 0 {
		t.Errorf("a raw delete outside any completed dream run is not dream's: got %v", got)
	}
}

func TestFindDreamLost_SkipsWhenTheUserForgotATwin(t *testing.T) {
	f := newDreamLostFixture(t)
	now := time.Now()
	f.run("run", "completed", now.Add(-2*time.Minute), now.Add(-time.Minute))

	f.save("dup", "text the user forgot")
	f.save("twin", "text the user forgot")
	if err := f.store.SoftDelete(context.Background(), "twin"); err != nil {
		t.Fatal(err)
	}
	f.proposeAndRawDelete("dup", "run")

	if got := f.lostIDs(); len(got) != 0 {
		t.Errorf("restoring a copy of something the user forgot resurrects it: got %v", got)
	}
}

func TestFindDreamLost_OneCopyPerProjectAndHash(t *testing.T) {
	f := newDreamLostFixture(t)
	now := time.Now()
	f.run("run", "completed", now.Add(-2*time.Minute), now.Add(-time.Minute))

	f.save("first", "same text twice")
	time.Sleep(10 * time.Millisecond)
	f.save("second", "same text twice")
	f.proposeAndRawDelete("first", "run")
	f.proposeAndRawDelete("second", "run")

	got := f.lostIDs()
	if len(got) != 1 || got[0] != "first" {
		t.Errorf("restore one copy per (project, hash), the oldest: got %v", got)
	}
}

// A purge (or curation clean) deletes a twin with a raw UPDATE outside any
// dream run: the content was removed on purpose, so dream's earlier delete of
// the other copy must not be undone either.
func TestFindDreamLost_SkipsWhenATwinWasDeletedOutsideDream(t *testing.T) {
	f := newDreamLostFixture(t)
	now := time.Now()
	f.run("run", "completed", now.Add(-48*time.Hour), now.Add(-47*time.Hour))

	f.save("dup", "purged later")
	f.save("twin", "purged later")
	// dream deleted dup during the old run...
	if _, err := f.db.Exec(
		"INSERT INTO dream_actions (id, run_id, memory_id, related_memory_id, action_type, confidence, reason, status) VALUES ('a-dup', 'run', 'dup', 'twin', 'dedup', 1.0, 'test', 'proposed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec("UPDATE memories SET deleted_at = datetime('now', '-47 hours', '+5 minutes') WHERE id = 'dup'"); err != nil {
		t.Fatal(err)
	}
	// ...and the user purged the surviving twin today.
	if _, err := f.db.Exec("UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'twin'"); err != nil {
		t.Fatal(err)
	}

	if got := f.lostIDs(); len(got) != 0 {
		t.Errorf("content the user purged must stay deleted: got %v", got)
	}
}
