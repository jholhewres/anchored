package project

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Consolidation repairs the project rows that worktree detection used to
// create. Before the fix, identity came from `git rev-parse --show-toplevel`,
// which in a linked worktree returns the worktree's own path; since projects
// are looked up by exact path, every worktree got a row of its own and started
// with no memory, while the repository's history stayed under the main
// checkout.
//
// Fixing detection only helps from now on. Folding the rows that already
// fragmented needs git and the filesystem to prove which path is a worktree of
// which repository, so it cannot be a SQL migration — it lives here, behind an
// explicit command.

// MergeKind distinguishes the two repairs.
type MergeKind string

const (
	// KindMerge folds a worktree's project into the repository's existing one.
	KindMerge MergeKind = "merge"
	// KindRepath moves a project onto its repository's path when no row exists
	// there yet — the worktree was seen but the main checkout never was.
	KindRepath MergeKind = "repath"
)

// Merge is one repair: everything under From is reattributed to Into.
type Merge struct {
	Kind MergeKind
	From Project
	// Into is the destination project. For KindRepath it is zero and IntoPath
	// carries the path From will be moved to.
	Into     Project
	IntoPath string
	// Rows counts what moves, per table, so a dry run can be read before
	// anything is written.
	Rows map[string]int
}

// Total is how many rows this repair reattributes.
func (m Merge) Total() int {
	n := 0
	for _, c := range m.Rows {
		n += c
	}
	return n
}

// Report is the plan. Nothing in it has been written.
type Report struct {
	Scanned int
	Merges  []Merge
	// Missing are projects whose path is gone from disk. Whether they were
	// worktrees cannot be proven without the filesystem, so they are reported
	// and left alone rather than guessed at.
	//
	// remote_key is deliberately not used to rescue them: it is stable across
	// worktrees, but two independent clones of the same remote share it too, so
	// folding on that signal would merge exactly the checkouts a person keeps
	// apart on purpose.
	Missing []Stranded
}

// Stranded is a project whose directory is gone, with how much memory is
// attributed to it — so the report can say what is sitting there rather than
// just naming a path.
type Stranded struct {
	Project  Project
	Memories int
}

// Affected is how many rows the whole plan would move.
func (r *Report) Affected() int {
	n := 0
	for _, m := range r.Merges {
		n += m.Total()
	}
	return n
}

// projectIDTables are every table that attributes a row to a project. Derived
// at runtime so a new table cannot be silently left behind by this command.
func projectIDTables(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var withProject []string
	for _, t := range tables {
		has, _, err := projectIDColumn(db, t)
		if err != nil {
			return nil, err
		}
		if has {
			withProject = append(withProject, t)
		}
	}
	sort.Strings(withProject)
	return withProject, nil
}

// projectIDColumn reports whether the table has a project_id column and whether
// that column is part of the primary key. A table keyed by project_id holds at
// most one row per project, so merging two of them is a collision rather than a
// move: the destination's row is the one that survives.
func projectIDColumn(db *sql.DB, table string) (has bool, isPK bool, err error) {
	rows, err := db.Query(`SELECT name, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var pk int
		if err := rows.Scan(&name, &pk); err != nil {
			return false, false, err
		}
		if name == "project_id" {
			return true, pk > 0, nil
		}
	}
	return false, false, rows.Err()
}

// PlanConsolidate works out which project rows are worktrees of another and
// what folding them would move. It only reads.
func (d *Detector) PlanConsolidate() (*Report, error) {
	rows, err := d.db.Query(`SELECT id, name, path, COALESCE(remote_key, '') FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var all []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.RemoteKey); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	byPath := make(map[string]Project, len(all))
	for _, p := range all {
		byPath[filepath.Clean(p.Path)] = p
	}

	tables, err := projectIDTables(d.db)
	if err != nil {
		return nil, fmt.Errorf("inspect schema: %w", err)
	}

	report := &Report{Scanned: len(all)}
	for _, p := range all {
		path := filepath.Clean(p.Path)

		if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
			var n int
			if err := d.db.QueryRow(
				`SELECT COUNT(*) FROM memories WHERE project_id = ?`, p.ID,
			).Scan(&n); err != nil {
				return nil, fmt.Errorf("count stranded memories: %w", err)
			}
			report.Missing = append(report.Missing, Stranded{Project: p, Memories: n})
			continue
		}

		// linkedWorktreeRoot returns "" for anything that is not a linked
		// worktree, so a main checkout, a submodule and a bare repository are
		// all left alone.
		root := linkedWorktreeRoot(path)
		if root == "" || filepath.Clean(root) == path {
			continue
		}
		root = filepath.Clean(root)

		m := Merge{From: p}
		if dest, ok := byPath[root]; ok {
			if dest.ID == p.ID {
				continue
			}
			m.Kind = KindMerge
			m.Into = dest
			m.IntoPath = dest.Path
		} else {
			m.Kind = KindRepath
			m.IntoPath = root
		}

		if m.Kind == KindMerge {
			m.Rows = make(map[string]int, len(tables))
			for _, t := range tables {
				var n int
				q := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE project_id = ?`, t)
				if err := d.db.QueryRow(q, p.ID).Scan(&n); err != nil {
					return nil, fmt.Errorf("count %s: %w", t, err)
				}
				if n > 0 {
					m.Rows[t] = n
				}
			}
		}
		report.Merges = append(report.Merges, m)
	}

	sort.Slice(report.Merges, func(i, j int) bool {
		return report.Merges[i].From.Path < report.Merges[j].From.Path
	})
	return report, nil
}

// ApplyConsolidate writes the plan. Everything happens in one transaction: the
// database either ends up fully consolidated or untouched.
func (d *Detector) ApplyConsolidate(r *Report) error {
	if r == nil || len(r.Merges) == 0 {
		return nil
	}

	tables, err := projectIDTables(d.db)
	if err != nil {
		return fmt.Errorf("inspect schema: %w", err)
	}
	pkTable := make(map[string]bool, len(tables))
	for _, t := range tables {
		_, isPK, err := projectIDColumn(d.db, t)
		if err != nil {
			return err
		}
		pkTable[t] = isPK
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, m := range r.Merges {
		if m.Kind == KindRepath {
			if _, err := tx.Exec(`UPDATE projects SET path = ? WHERE id = ?`, m.IntoPath, m.From.ID); err != nil {
				return fmt.Errorf("repath %s: %w", m.From.Path, err)
			}
			continue
		}

		for _, t := range tables {
			if pkTable[t] {
				// One row per project by construction. The destination already
				// has its own, and it is the repository's — the worktree's copy
				// is derived state that would only collide.
				if _, err := tx.Exec(
					fmt.Sprintf(`UPDATE OR IGNORE %q SET project_id = ? WHERE project_id = ?`, t),
					m.Into.ID, m.From.ID,
				); err != nil {
					return fmt.Errorf("move %s: %w", t, err)
				}
				if _, err := tx.Exec(
					fmt.Sprintf(`DELETE FROM %q WHERE project_id = ?`, t), m.From.ID,
				); err != nil {
					return fmt.Errorf("prune %s: %w", t, err)
				}
				continue
			}
			if _, err := tx.Exec(
				fmt.Sprintf(`UPDATE %q SET project_id = ? WHERE project_id = ?`, t),
				m.Into.ID, m.From.ID,
			); err != nil {
				return fmt.Errorf("move %s: %w", t, err)
			}
		}

		if _, err := tx.Exec(`DELETE FROM projects WHERE id = ?`, m.From.ID); err != nil {
			return fmt.Errorf("drop project %s: %w", m.From.Path, err)
		}
	}

	return tx.Commit()
}
