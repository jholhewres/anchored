package dream

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type ConsolidationResult struct {
	Merged      int `json:"merged"`
	SoftDeleted int `json:"soft_deleted"`
	Flagged     int `json:"flagged"`
	Skipped     int `json:"skipped"`
}

// Ledger is how dream changes memories. Going through the temporal ledger
// means a delete leaves a tombstone revision that Restore can undo, cancels
// the memory's pending remote sync and keeps the vector cache consistent;
// a raw UPDATE of memories does none of that. *memory.Service and
// *memory.SQLiteStore both satisfy it.
type Ledger interface {
	SoftDeleteIfActive(ctx context.Context, id string) (bool, error)
	UpdateMetadata(ctx context.Context, id string, metadata any) error
}

type DreamConsolidator struct {
	db     *sql.DB
	ledger Ledger
	logger *slog.Logger
}

func NewConsolidator(db *sql.DB, ledger Ledger, logger *slog.Logger) *DreamConsolidator {
	if logger == nil {
		logger = slog.Default()
	}
	return &DreamConsolidator{db: db, ledger: ledger, logger: logger}
}

// checkDedupPair returns why deleting memoryID in favour of keeperID could
// lose data, or "" when it is safe: both must be live and belong to the same
// project and, when sameContent is set (dedup), carry the same content hash.
// Proposals are stored and may be applied long after analysis, over text
// edited since or from a near-duplicate guess, so this is re-checked every
// time instead of trusted from the report.
func (c *DreamConsolidator) checkDedupPair(ctx context.Context, memoryID, keeperID string, sameContent bool) (string, error) {
	if keeperID == "" {
		return "no keeper recorded for this duplicate", nil
	}
	if keeperID == memoryID {
		return "a memory cannot be its own keeper", nil
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT id, COALESCE(project_id, ''), COALESCE(content_hash, '') FROM memories WHERE id IN (?, ?) AND deleted_at IS NULL",
		memoryID, keeperID)
	if err != nil {
		return "", fmt.Errorf("load dedup pair: %w", err)
	}
	defer rows.Close()
	type row struct{ project, hash string }
	live := make(map[string]row, 2)
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.project, &r.hash); err != nil {
			return "", fmt.Errorf("scan dedup pair: %w", err)
		}
		live[id] = r
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("load dedup pair: %w", err)
	}
	target, targetLive := live[memoryID]
	keeper, keeperLive := live[keeperID]
	switch {
	case !targetLive:
		return "memory is missing or already deleted", nil
	case !keeperLive:
		return "keeper is missing or deleted", nil
	case target.project != keeper.project:
		return "keeper belongs to another project", nil
	case sameContent && (target.hash == "" || target.hash != keeper.hash):
		return "contents differ; only identical memories are deduplicated", nil
	}
	return "", nil
}

func (c *DreamConsolidator) markApplied(ctx context.Context, actionID string) error {
	_, err := c.db.ExecContext(ctx,
		"UPDATE dream_actions SET status = 'applied', applied_at = CURRENT_TIMESTAMP WHERE id = ? AND status = 'proposed'",
		actionID)
	return err
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

			refusal, err := c.checkDedupPair(ctx, action.MemoryID, action.RelatedMemoryID, true)
			if err != nil {
				c.logger.Warn("dedup check failed", "id", action.MemoryID, "error", err)
				result.Skipped++
				continue
			}
			if refusal != "" {
				c.logger.Debug("dedup skipped", "id", action.MemoryID, "keeper", action.RelatedMemoryID, "reason", refusal)
				result.Skipped++
				continue
			}
			deleted, err := c.ledger.SoftDeleteIfActive(ctx, action.MemoryID)
			if err != nil {
				c.logger.Warn("soft-delete failed", "id", action.MemoryID, "error", err)
				result.Skipped++
				continue
			}
			if !deleted {
				result.Skipped++
				continue
			}
			result.SoftDeleted++
			deletions++
			if action.ID != "" {
				if err := c.markApplied(ctx, action.ID); err != nil {
					c.logger.Warn("mark dream action applied failed", "action", action.ID, "error", err)
				}
			}

		case "contradiction":
			result.Flagged++
			// Never auto-resolve contradictions

		default:
			result.Skipped++
		}
	}

	return result, nil
}

type ApplyActionResult struct {
	ActionID   string `json:"action_id"`
	ActionType string `json:"action_type"`
	MemoryID   string `json:"memory_id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

func (c *DreamConsolidator) ApplyAction(ctx context.Context, actionID string) (*ApplyActionResult, error) {
	action, err := GetAction(ctx, c.db, actionID)
	if err != nil {
		return nil, fmt.Errorf("lookup action: %w", err)
	}
	if action == nil {
		return nil, fmt.Errorf("action %q not found", actionID)
	}
	if action.Status != "proposed" {
		return nil, fmt.Errorf("action %q has status %q, cannot apply (only \"proposed\" actions are eligible)", actionID, action.Status)
	}

	switch action.ActionType {
	case "dedup":
		refusal, err := c.checkDedupPair(ctx, action.MemoryID, action.RelatedMemoryID, true)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return nil, fmt.Errorf("refusing dedup %q: %s", actionID, refusal)
		}
		deleted, err := c.ledger.SoftDeleteIfActive(ctx, action.MemoryID)
		if err != nil {
			return nil, fmt.Errorf("soft-delete memory %q: %w", action.MemoryID, err)
		}
		if !deleted {
			return nil, fmt.Errorf("memory %q was deleted meanwhile; nothing applied", action.MemoryID)
		}
		if err := c.markApplied(ctx, actionID); err != nil {
			return nil, fmt.Errorf("update action status: %w", err)
		}

		return &ApplyActionResult{
			ActionID:   actionID,
			ActionType: action.ActionType,
			MemoryID:   action.MemoryID,
			Status:     "applied",
			Message:    fmt.Sprintf("soft-deleted memory %q (dedup, confidence=%.2f)", action.MemoryID, action.Confidence),
		}, nil

	case "contradiction":
		return nil, fmt.Errorf("contradiction actions require manual review and cannot be auto-applied")

	case "supersede":
		relatedID := action.RelatedMemoryID
		if relatedID == "" {
			return nil, fmt.Errorf("supersede action requires related_memory_id")
		}

		var metaJSON string
		err := c.db.QueryRowContext(ctx,
			"SELECT COALESCE(metadata, '') FROM memories WHERE id = ? AND deleted_at IS NULL", action.MemoryID,
		).Scan(&metaJSON)
		if err != nil {
			return nil, fmt.Errorf("lookup memory for supersede: %w", err)
		}

		var meta map[string]any
		if metaJSON != "" && metaJSON != "null" {
			if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
				return nil, fmt.Errorf("parse metadata for supersede: %w", err)
			}
		}
		if meta == nil {
			meta = make(map[string]any)
		}

		existing, _ := meta["supersedes"].([]any)
		existing = append(existing, relatedID)
		meta["supersedes"] = existing

		if err := c.ledger.UpdateMetadata(ctx, action.MemoryID, meta); err != nil {
			return nil, fmt.Errorf("update superseded metadata: %w", err)
		}
		if err := c.markApplied(ctx, actionID); err != nil {
			return nil, fmt.Errorf("update action status: %w", err)
		}

		return &ApplyActionResult{
			ActionID:   actionID,
			ActionType: action.ActionType,
			MemoryID:   action.MemoryID,
			Status:     "applied",
			Message:    fmt.Sprintf("memory %q now supersedes %q", action.MemoryID, relatedID),
		}, nil

	case "merge":
		relatedID := action.RelatedMemoryID
		if relatedID == "" {
			return nil, fmt.Errorf("merge action requires related_memory_id")
		}
		// merge keeps MemoryID and deletes the related memory it absorbs.
		refusal, err := c.checkDedupPair(ctx, relatedID, action.MemoryID, false)
		if err != nil {
			return nil, err
		}
		if refusal != "" {
			return nil, fmt.Errorf("refusing merge %q: %s", actionID, refusal)
		}

		var metaJSON string
		err = c.db.QueryRowContext(ctx,
			"SELECT COALESCE(metadata, '') FROM memories WHERE id = ? AND deleted_at IS NULL", action.MemoryID,
		).Scan(&metaJSON)
		if err != nil {
			return nil, fmt.Errorf("lookup memory for merge: %w", err)
		}

		var meta map[string]any
		if metaJSON != "" && metaJSON != "null" {
			if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
				return nil, fmt.Errorf("parse metadata for merge: %w", err)
			}
		}
		if meta == nil {
			meta = make(map[string]any)
		}

		existing, _ := meta["consolidates"].([]any)
		existing = append(existing, relatedID)
		meta["consolidates"] = existing

		if err := c.ledger.UpdateMetadata(ctx, action.MemoryID, meta); err != nil {
			return nil, fmt.Errorf("update consolidation metadata: %w", err)
		}
		if _, err := c.ledger.SoftDeleteIfActive(ctx, relatedID); err != nil {
			return nil, fmt.Errorf("soft-delete merged memory %q: %w", relatedID, err)
		}
		if err := c.markApplied(ctx, actionID); err != nil {
			return nil, fmt.Errorf("update action status: %w", err)
		}

		return &ApplyActionResult{
			ActionID:   actionID,
			ActionType: action.ActionType,
			MemoryID:   action.MemoryID,
			Status:     "applied",
			Message:    fmt.Sprintf("memory %q consolidates %q (soft-deleted)", action.MemoryID, relatedID),
		}, nil

	case "synthesize":
		return c.applySynthesize(ctx, action, actionID)

	default:
		return nil, fmt.Errorf("unknown action type %q", action.ActionType)
	}
}

// applySynthesize consolidates a near-dup cluster (Feature E): a new summary
// memory is created from the members' content (deterministic recap, no model)
// and the raw members are DEMOTED — low_signal/consolidated, advisory and
// reversible — never deleted. The summary records the member IDs in
// metadata.consolidated; embedding stays NULL for the curation worker.
func (c *DreamConsolidator) applySynthesize(ctx context.Context, action *DreamActionRecord, actionID string) (*ApplyActionResult, error) {
	memberIDs := append([]string{action.MemoryID}, splitCSV(action.RelatedMemoryID)...)
	if len(memberIDs) < 3 {
		return nil, fmt.Errorf("synthesize requires a cluster of >= 3 members, got %d", len(memberIDs))
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(memberIDs)), ",")
	args := make([]any, len(memberIDs))
	for i, id := range memberIDs {
		args[i] = id
	}
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, COALESCE(project_id, ''), category, content FROM memories
		WHERE deleted_at IS NULL AND id IN (`+placeholders+`)
		ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("load cluster members: %w", err)
	}
	defer rows.Close()

	type member struct{ id, projectID, category, content string }
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.id, &m.projectID, &m.category, &m.content); err != nil {
			continue
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load cluster members: %w", err)
	}
	if len(members) < 3 {
		return nil, fmt.Errorf("cluster shrank below 3 live members (%d), refusing to synthesize", len(members))
	}

	// Deterministic synthesis: newest member is the base, the others append
	// as compact bullets. No model in the loop — the synthesis is honest
	// about being a recap, not a rewrite.
	var b strings.Builder
	fmt.Fprintf(&b, "Consolidated from %d related memories: %s", len(members), strings.TrimSpace(members[0].content))
	for _, m := range members[1:] {
		fmt.Fprintf(&b, " | %s", truncate(strings.TrimSpace(m.content), 240))
	}
	content := b.String()

	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.id
	}
	metaJSON, err := json.Marshal(map[string]any{
		"memory_type":  "semantic",
		"kind":         "summary",
		"origin":       "dream",
		"consolidated": ids,
		"supersedes":   ids,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal synthesis metadata: %w", err)
	}

	// The three writes below (create summary, demote members, mark action
	// applied) must land together — a partial apply would leave demoted
	// members without their synthesis, or a synthesis nobody points at.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin synthesize tx: %w", err)
	}
	defer tx.Rollback()

	newID := newDreamID()
	revisionID := newDreamID()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, content_hash, keywords, embedding,
		                      source, created_at, updated_at, access_count, metadata, sync_dirty,
		                      logical_id, current_revision_id)
		VALUES (?, NULLIF(?, ''), 'summary', ?, '', '[]', NULL, 'dream_consolidation',
		        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 0, ?, 0, ?, ?)`,
		newID, members[0].projectID, content, string(metaJSON), newID, revisionID); err != nil {
		return nil, fmt.Errorf("insert synthesis memory: %w", err)
	}

	// Curation resolves a memory through memories.current_revision_id and the
	// embedding queue joins on it, so a synthesis without a base revision would
	// never be scored nor embedded.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memory_revisions (
			revision_id, memory_id, logical_id, project_id, category,
			content, content_hash, keywords, source, metadata,
			memory_created_at, memory_updated_at,
			temporal_mode, is_tombstone, valid_from, valid_to, system_from, system_to)
		VALUES (?, ?, ?, NULLIF(?, ''), 'summary', ?, '', '[]', 'dream_consolidation', ?,
		        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'supersede', FALSE, ?, NULL, ?, NULL)`,
		revisionID, newID, newID, members[0].projectID, content, string(metaJSON),
		time.Now().UTC().UnixNano(), time.Now().UTC().UnixNano()); err != nil {
		return nil, fmt.Errorf("insert synthesis base revision: %w", err)
	}

	// Demote (never delete) the raw members. curation_rule=consolidated is
	// exempt from RecurateMetadata's lift, so the demotion is structural and
	// stable until explicitly undone.
	demoteArgs := make([]any, 0, len(ids)+1)
	demoteArgs = append(demoteArgs, newID)
	for _, id := range ids {
		demoteArgs = append(demoteArgs, id)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET metadata = json_set(
			COALESCE(NULLIF(NULLIF(metadata, ''), 'null'), '{}'),
			'$.curation_status', 'low_signal',
			'$.curation_rule', 'consolidated',
			'$.consolidated_into', ?
		), updated_at = CURRENT_TIMESTAMP
		WHERE id IN (`+placeholders+`)`, demoteArgs...); err != nil {
		return nil, fmt.Errorf("demote cluster members: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE dream_actions SET status = 'applied', applied_at = CURRENT_TIMESTAMP WHERE id = ?",
		actionID); err != nil {
		return nil, fmt.Errorf("update action status: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit synthesize tx: %w", err)
	}

	return &ApplyActionResult{
		ActionID:   actionID,
		ActionType: action.ActionType,
		MemoryID:   newID,
		Status:     "applied",
		Message:    fmt.Sprintf("synthesized %d memories into summary %q (members demoted, not deleted)", len(members), newID),
	}, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// newDreamID generates a random 32-hex id, matching the format of the rest
// of the store.
func newDreamID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("dream-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
