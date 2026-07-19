package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jholhewres/anchored/pkg/contextbudget"
)

// recall_logs / project_baseline power `anchored stats --tokens`: every context
// injection records how many tokens were sent vs. the static-context baseline
// (CLAUDE.md/AGENTS.md + skills) it replaced. All writes here are best-effort —
// a hook must never fail or slow down because telemetry couldn't be recorded.

// ensureRecallSchema creates the telemetry tables if they're absent. Hooks open
// the DB directly and may run before the memory service migrates it, so we
// create-if-missing at the write site rather than assuming the schema exists.
func ensureRecallSchema(db *sql.DB) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS recall_logs (
	id TEXT PRIMARY KEY,
	project_id TEXT,
	surface TEXT,
	tokens_injected INTEGER NOT NULL DEFAULT 0,
	baseline_tokens INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_recall_logs_project_created ON recall_logs(project_id, created_at);
CREATE TABLE IF NOT EXISTS project_baseline (
	project_id TEXT PRIMARY KEY,
	baseline_tokens INTEGER NOT NULL DEFAULT 0,
	computed_on TEXT NOT NULL DEFAULT ''
);`
	_, err := db.Exec(ddl)
	return err
}

// recordRecall writes one telemetry row for a context injection. Best-effort:
// any error is swallowed so the calling hook is never affected. Runs
// synchronously (a single local sqlite write is sub-millisecond) because hook
// processes are short-lived — a background goroutine would be killed on exit
// before it could commit.
func recordRecall(db *sql.DB, projectID, surface string, tokensInjected, baselineTokens int) {
	if db == nil || tokensInjected <= 0 {
		return
	}
	if err := ensureRecallSchema(db); err != nil {
		return
	}
	_, _ = db.Exec(
		`INSERT INTO recall_logs (id, project_id, surface, tokens_injected, baseline_tokens) VALUES (?, ?, ?, ?, ?)`,
		newHookID(), projectID, surface, tokensInjected, baselineTokens,
	)
}

// projectBaselineTokens returns the static-context token count for a project,
// recomputed from disk at most once per day and cached in project_baseline.
// Returns 0 on any error — a missing baseline just means "no comparison yet".
func projectBaselineTokens(db *sql.DB, projectID, projectPath string, now time.Time) int {
	if db == nil || projectPath == "" {
		return 0
	}
	if err := ensureRecallSchema(db); err != nil {
		return 0
	}
	today := now.Format("2006-01-02")

	var cachedTokens int
	var computedOn string
	err := db.QueryRowContext(context.Background(),
		`SELECT baseline_tokens, computed_on FROM project_baseline WHERE project_id = ?`, projectID,
	).Scan(&cachedTokens, &computedOn)
	if err == nil && computedOn == today {
		return cachedTokens
	}

	tokens := computeBaselineFromFiles(projectPath)
	_, _ = db.Exec(
		`INSERT INTO project_baseline (project_id, baseline_tokens, computed_on) VALUES (?, ?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET baseline_tokens = excluded.baseline_tokens, computed_on = excluded.computed_on`,
		projectID, tokens, today,
	)
	return tokens
}

// computeBaselineFromFiles sums the approximate token cost of the static
// context a project would otherwise load every session: CLAUDE.md / AGENTS.md
// at the root plus every .claude/skills/**/SKILL.md.
func computeBaselineFromFiles(projectPath string) int {
	total := 0
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		total += fileTokens(filepath.Join(projectPath, name))
	}
	skillsDir := filepath.Join(projectPath, ".claude", "skills")
	_ = filepath.WalkDir(skillsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree → skip, never fail the walk
		}
		if !d.IsDir() && d.Name() == "SKILL.md" {
			total += fileTokens(path)
		}
		return nil
	})
	return total
}

// fileTokens returns the approximate token count of a file, or 0 if it can't
// be read.
func fileTokens(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return contextbudget.ApproxTokens(string(data))
}

// recallTokenSummary aggregates the last `days` of recall telemetry for
// `anchored stats --tokens`.
type recallTokenSummary struct {
	Injections     int
	InjectedTokens int
	BaselineTokens int
}

// SavingsPct is the share of baseline tokens avoided (0 when no baseline).
func (s recallTokenSummary) SavingsPct() float64 {
	if s.BaselineTokens <= 0 {
		return 0
	}
	saved := s.BaselineTokens - s.InjectedTokens
	if saved < 0 {
		saved = 0
	}
	return float64(saved) / float64(s.BaselineTokens) * 100
}

// queryRecallSummary reads recall_logs for the trailing window.
func queryRecallSummary(db *sql.DB, days int) (recallTokenSummary, error) {
	var s recallTokenSummary
	if db == nil {
		return s, nil
	}
	if err := ensureRecallSchema(db); err != nil {
		return s, err
	}
	row := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COALESCE(SUM(tokens_injected),0), COALESCE(SUM(baseline_tokens),0)
		 FROM recall_logs WHERE created_at > datetime('now', ?)`,
		fmt.Sprintf("-%d days", days),
	)
	if err := row.Scan(&s.Injections, &s.InjectedTokens, &s.BaselineTokens); err != nil {
		return recallTokenSummary{}, err
	}
	return s, nil
}
