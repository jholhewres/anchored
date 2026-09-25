package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// transcriptImportSources are the sources the importer writes raw transcript
// turns under. Most of them are narration rather than knowledge, so restoring
// them is opt-in.
var transcriptImportSources = map[string]bool{"claude-code": true, "opencode": true}

// dreamLostMemory is a memory dream deleted as a duplicate even though no live
// copy of it remains in its project: before v0.20 exact dedup grouped by
// content hash across projects, so deleting "the duplicate" could remove the
// only copy a project had.
type dreamLostMemory struct {
	ID        string
	Source    string
	ProjectID string
	CreatedAt string
	Content   string
}

// findDreamLost selects memories that dream deleted and that have no live twin
// (same content hash) in the same project. A dream delete is recognised by
// three traces it leaves together: deleted_at set by a raw UPDATE while the
// current revision is not a tombstone; a dedup/merge proposal naming the
// memory as the one to remove; and a delete time inside the window of a
// completed dream run, which records itself just before applying proposals.
// purge, curation clean and directives rm also delete raw, and proposals from
// dry runs cover most of the corpus, so the run window is what tells dream's
// deletes from deliberate ones. A candidate is left alone when any twin was
// deleted by something other than dream (forgotten through the ledger, or
// purged, cleaned or removed raw outside a run): that content was removed on
// purpose. Only the oldest copy of each (project, hash) group comes back; the
// others would be duplicates again.
func findDreamLost(ctx context.Context, db *sql.DB, includeImported bool) ([]dreamLostMemory, error) {
	rows, err := db.QueryContext(ctx, `
		WITH dreamed AS (
			SELECT memory_id AS id FROM dream_actions WHERE action_type = 'dedup'
			UNION
			SELECT related_memory_id FROM dream_actions WHERE action_type = 'merge'
		),
		live AS MATERIALIZED (
			SELECT DISTINCT COALESCE(project_id, '') AS pid, content_hash AS h
			FROM memories
			WHERE deleted_at IS NULL AND COALESCE(content_hash, '') != ''
		),
		applying AS MATERIALIZED (
			SELECT julianday(started_at) AS start_jd, julianday(finished_at) + 30.0 / 1440 AS end_jd
			FROM dream_runs
			WHERE status = 'completed' AND started_at IS NOT NULL AND finished_at IS NOT NULL
		)
		SELECT m.id, COALESCE(m.source, ''), COALESCE(m.project_id, ''), COALESCE(m.content_hash, ''),
		       COALESCE(CAST(m.created_at AS TEXT), ''), m.content
		FROM memories m
		JOIN dreamed d ON d.id = m.id
		JOIN memory_revisions r ON r.revision_id = m.current_revision_id AND NOT r.is_tombstone
		WHERE m.deleted_at IS NOT NULL
		  AND EXISTS (
			SELECT 1 FROM applying a
			WHERE julianday(m.deleted_at) BETWEEN a.start_jd AND a.end_jd
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM live
			WHERE live.pid = COALESCE(m.project_id, '') AND live.h = m.content_hash
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM memories t
			JOIN memory_revisions tr ON tr.revision_id = t.current_revision_id
			WHERE t.content_hash = m.content_hash AND t.id != m.id
			  AND COALESCE(t.project_id, '') = COALESCE(m.project_id, '')
			  AND t.deleted_at IS NOT NULL
			  AND (tr.is_tombstone OR NOT EXISTS (
				SELECT 1 FROM applying a
				WHERE julianday(t.deleted_at) BETWEEN a.start_jd AND a.end_jd
			  ))
		  )
		ORDER BY m.created_at, m.id`)
	if err != nil {
		return nil, fmt.Errorf("find dream-lost memories: %w", err)
	}
	defer rows.Close()

	var out []dreamLostMemory
	seen := map[string]bool{}
	for rows.Next() {
		var m dreamLostMemory
		var hash string
		if err := rows.Scan(&m.ID, &m.Source, &m.ProjectID, &hash, &m.CreatedAt, &m.Content); err != nil {
			return nil, fmt.Errorf("scan dream-lost memory: %w", err)
		}
		if !includeImported && transcriptImportSources[m.Source] {
			continue
		}
		if key := m.ProjectID + "\x00" + hash; hash != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func runRestore(args []string) {
	fs := newFlagSet("restore")
	configPath := fs.String("config", "", "path to config file")
	ids := fs.String("id", "", "restore these memories (comma-separated ids)")
	dreamLost := fs.Bool("dream-lost", false, "restore memories dream deleted with no live copy left in their project")
	includeImported := fs.Bool("include-imported", false, "with --dream-lost: also restore raw transcript imports (claude-code, opencode)")
	yes := fs.Bool("yes", false, "with --dream-lost: actually restore (without it, only report)")
	fs.Parse(args)

	if (*ids == "") == !*dreamLost {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  anchored restore --id <id>[,<id>...]")
		fmt.Fprintln(os.Stderr, "  anchored restore --dream-lost [--include-imported] [--yes]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "--dream-lost reports what it would restore and changes nothing until --yes is passed.")
		os.Exit(1)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()
	ctx := context.Background()

	var targets []string
	if *ids != "" {
		for _, id := range strings.Split(*ids, ",") {
			if id = strings.TrimSpace(id); id != "" {
				targets = append(targets, id)
			}
		}
	} else {
		lost, err := findDreamLost(ctx, svc.StoreDB(), *includeImported)
		if err != nil {
			fmt.Fprintf(os.Stderr, "restore error: %v\n", err)
			os.Exit(1)
		}
		printDreamLost(lost)
		if !*yes {
			if len(lost) > 0 {
				fmt.Println("\nNothing changed. Re-run with --yes to restore them.")
			}
			return
		}
		for _, m := range lost {
			targets = append(targets, m.ID)
		}
	}

	restored, unchanged, failed := 0, 0, 0
	for _, id := range targets {
		changed, err := svc.RestoreDeleted(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "restore %s: %v\n", id, err)
			failed++
			continue
		}
		if changed {
			restored++
		} else {
			unchanged++
		}
	}
	fmt.Printf("Restored %d memories (%d already live or missing).\n", restored, unchanged)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "%d could not be restored.\n", failed)
		os.Exit(1)
	}
}

func printDreamLost(lost []dreamLostMemory) {
	bySource := map[string]int{}
	for _, m := range lost {
		bySource[m.Source]++
	}
	fmt.Printf("%d memories deleted by dream with no live copy left in their project\n", len(lost))
	for source, n := range bySource {
		fmt.Printf("  %-20s %d\n", source, n)
	}
	for i, m := range lost {
		if i == 10 {
			fmt.Printf("  … and %d more\n", len(lost)-10)
			break
		}
		created := m.CreatedAt
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			created = t.Format("2006-01-02")
		} else if len(created) >= 10 {
			created = created[:10]
		}
		fmt.Printf("  %s  %s  %-12s %s\n", m.ID, created, m.Source, previewLine(m.Content, 90))
	}
}

func previewLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
