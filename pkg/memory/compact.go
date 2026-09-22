package memory

import (
	"context"
	"database/sql"
	"fmt"
)

// CompactOptions selects how aggressively Compact reclaims space.
type CompactOptions struct {
	// KeepHistory leaves memory_revisions untouched and prunes only derived
	// data. Derived rows are regenerable caches, so the default sweep is
	// lossless either way; this flag additionally preserves every redundant
	// revision for anyone auditing the raw ledger.
	KeepHistory bool

	// DryRun measures what would be removed without removing anything.
	DryRun bool

	// Vacuum rewrites the database file afterwards so freed pages return to the
	// filesystem. It needs free disk space roughly equal to the final size.
	Vacuum bool
}

// CompactStats reports what a sweep removed (or would remove).
type CompactStats struct {
	RedundantRevisions int64
	SupersededVectors  int64
	CompletedJobs      int64
	SizeBefore         int64
	SizeAfter          int64
}

// Compact reclaims space taken by rows that carry no information.
//
// Three families accumulate, all of them downstream of a save path that used to
// append a fresh revision even when nothing about the memory had changed:
//
//   - redundant revisions — successive entries for one memory holding the same
//     content_hash AND the same metadata. The first entry records when that
//     state began; the ones after it record nothing. The bitemporal invariant is
//     a non-overlap check, which deletion can only ever satisfy further.
//   - superseded embedding vectors — every revision got a verbatim copy of the
//     memory's vector, so a memory written N times stored N identical copies.
//     Only the copy attached to the current revision is ever read.
//   - completed processing jobs — done rows whose revision is gone. Nothing
//     re-reads them; a revision's vector, not its job row, is what keeps it from
//     being re-enqueued.
func Compact(ctx context.Context, db *sql.DB, opts CompactOptions) (CompactStats, error) {
	var stats CompactStats

	before, err := databaseSize(ctx, db)
	if err != nil {
		return stats, err
	}
	stats.SizeBefore = before
	stats.SizeAfter = before

	if !opts.KeepHistory {
		n, err := sweep(ctx, db, opts.DryRun, redundantRevisionsCount, redundantRevisionsDelete)
		if err != nil {
			return stats, fmt.Errorf("prune redundant revisions: %w", err)
		}
		stats.RedundantRevisions = n
	}

	// Runs after the revision sweep: deleting a revision cascades its vectors,
	// so this only has to catch copies whose revision survived.
	n, err := sweep(ctx, db, opts.DryRun, supersededVectorsCount, supersededVectorsDelete)
	if err != nil {
		return stats, fmt.Errorf("prune superseded vectors: %w", err)
	}
	stats.SupersededVectors = n

	n, err = sweep(ctx, db, opts.DryRun, completedJobsCount, completedJobsDelete)
	if err != nil {
		return stats, fmt.Errorf("prune completed jobs: %w", err)
	}
	stats.CompletedJobs = n

	if opts.DryRun {
		return stats, nil
	}

	if opts.Vacuum {
		// VACUUM cannot run inside a transaction and rewrites the whole file.
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			return stats, fmt.Errorf("vacuum: %w", err)
		}
	}

	after, err := databaseSize(ctx, db)
	if err != nil {
		return stats, err
	}
	stats.SizeAfter = after
	return stats, nil
}

func sweep(ctx context.Context, db *sql.DB, dryRun bool, countSQL, deleteSQL string) (int64, error) {
	if dryRun {
		var n int64
		if err := db.QueryRowContext(ctx, countSQL).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	res, err := db.ExecContext(ctx, deleteSQL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// redundantRevisionSelect names every revision that repeats a state already
// recorded for the same memory. Within each (memory_id, content_hash, metadata)
// group the earliest revision survives, and the memory's current revision is
// excluded unconditionally — losing it would orphan the memories row.
// Revisions still referenced by an undelivered outbox envelope are kept so a
// pending remote sync is never cut out from under itself.
const redundantRevisionSelect = `
	SELECT r.revision_id
	FROM memory_revisions r
	JOIN (
		SELECT memory_id, content_hash, COALESCE(metadata, '') AS meta,
		       MIN(system_from) AS first_seen
		FROM memory_revisions
		GROUP BY memory_id, content_hash, COALESCE(metadata, '')
		HAVING COUNT(*) > 1
	) g
	  ON g.memory_id = r.memory_id
	 AND g.content_hash IS r.content_hash
	 AND g.meta = COALESCE(r.metadata, '')
	 AND r.system_from > g.first_seen
	WHERE NOT EXISTS (
		SELECT 1 FROM memories m WHERE m.current_revision_id = r.revision_id
	)
	  AND NOT EXISTS (
		SELECT 1 FROM remote_outbox o
		WHERE o.revision_id = r.revision_id
		  AND o.state IN ('pending', 'processing')
	)`

var (
	redundantRevisionsCount  = `SELECT COUNT(*) FROM (` + redundantRevisionSelect + `)`
	redundantRevisionsDelete = `DELETE FROM memory_revisions WHERE revision_id IN (` +
		redundantRevisionSelect + `)`
)

// supersededVectorSelect names embedding copies attached to a revision that is
// no longer any memory's current one. Keying on revision currency rather than
// generation keeps a generation switch intact: vectors for the current revision
// survive in every generation until activation retires the old space.
const supersededVectorSelect = `
	SELECT v.rowid
	FROM memory_embedding_vectors v
	WHERE NOT EXISTS (
		SELECT 1 FROM memories m WHERE m.current_revision_id = v.revision_id
	)`

var (
	supersededVectorsCount  = `SELECT COUNT(*) FROM (` + supersededVectorSelect + `)`
	supersededVectorsDelete = `DELETE FROM memory_embedding_vectors WHERE rowid IN (` +
		supersededVectorSelect + `)`
)

const completedJobSelect = `
	SELECT j.id
	FROM memory_processing_jobs j
	WHERE j.state = 'done'
	  AND NOT EXISTS (
		SELECT 1 FROM memory_revisions r WHERE r.revision_id = j.revision_id
	)`

var (
	completedJobsCount  = `SELECT COUNT(*) FROM (` + completedJobSelect + `)`
	completedJobsDelete = `DELETE FROM memory_processing_jobs WHERE id IN (` +
		completedJobSelect + `)`
)

func databaseSize(ctx context.Context, db *sql.DB) (int64, error) {
	var pageCount, pageSize int64
	if err := db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, fmt.Errorf("page_count: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page_size: %w", err)
	}
	return pageCount * pageSize, nil
}
