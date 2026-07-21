package memory

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *SQLiteStore) DerivedWorkHealth(ctx context.Context) (DerivedWorkHealth, error) {
	processing, err := queueStateHealth(ctx, s.db, "memory_processing_jobs",
		[]string{"pending", "processing", "done", "failed"})
	if err != nil {
		return DerivedWorkHealth{}, fmt.Errorf("processing health: %w", err)
	}
	outbox, err := queueStateHealth(ctx, s.db, "remote_outbox",
		[]string{"pending", "processing", "delivered", "dead_letter"})
	if err != nil {
		return DerivedWorkHealth{}, fmt.Errorf("outbox health: %w", err)
	}
	return DerivedWorkHealth{Processing: processing, Outbox: outbox}, nil
}

func queueStateHealth(ctx context.Context, db *sql.DB, table string, states []string) (QueueStateHealth, error) {
	health := QueueStateHealth{Counts: make(map[string]int64, len(states))}
	for _, state := range states {
		health.Counts[state] = 0
	}
	rows, err := db.QueryContext(ctx, "SELECT state, COUNT(*) FROM "+table+" GROUP BY state")
	if err != nil {
		return QueueStateHealth{}, err
	}
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			rows.Close()
			return QueueStateHealth{}, err
		}
		health.Counts[state] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return QueueStateHealth{}, err
	}
	if err := rows.Close(); err != nil {
		return QueueStateHealth{}, err
	}
	var oldest sql.NullInt64
	if err := db.QueryRowContext(ctx,
		"SELECT MIN(created_at) FROM "+table+" WHERE state = 'pending'").Scan(&oldest); err != nil {
		return QueueStateHealth{}, err
	}
	if oldest.Valid {
		at := time.Unix(0, oldest.Int64).UTC()
		health.OldestPending = &at
	}
	return health, nil
}
