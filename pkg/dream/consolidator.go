package dream

import (
	"context"
	"database/sql"
	"log/slog"
)

type ConsolidationResult struct {
	Merged      int `json:"merged"`
	SoftDeleted int `json:"soft_deleted"`
	Flagged     int `json:"flagged"`
	Skipped     int `json:"skipped"`
}

type DreamConsolidator struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewConsolidator(db *sql.DB, logger *slog.Logger) *DreamConsolidator {
	if logger == nil {
		logger = slog.Default()
	}
	return &DreamConsolidator{db: db, logger: logger}
}

func (c *DreamConsolidator) Consolidate(ctx context.Context, report *DreamReport, cfg DreamConfig) (*ConsolidationResult, error) {
	result := &ConsolidationResult{}
	deletions := 0

	for _, action := range report.Actions {
		switch action.ActionType {
		case "dedup":
			if cfg.MaxDeletionsPerRun == 0 {
				result.Skipped++
				continue
			}
			if deletions >= cfg.MaxDeletionsPerRun {
				result.Skipped++
				continue
			}
			if action.Confidence < cfg.DedupThreshold {
				result.Skipped++
				continue
			}

			_, err := c.db.ExecContext(ctx,
				"UPDATE memories SET deleted_at = CURRENT_TIMESTAMP WHERE id = ? AND deleted_at IS NULL",
				action.MemoryID)
			if err != nil {
				c.logger.Warn("soft-delete failed", "id", action.MemoryID, "error", err)
				result.Skipped++
				continue
			}
			result.SoftDeleted++
			deletions++

		case "contradiction":
			result.Flagged++
			// Never auto-resolve contradictions

		default:
			result.Skipped++
		}
	}

	return result, nil
}
