package ctx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const minEvictionInterval = 1 * time.Minute

// EvictorConfig controls eviction behaviour.
type EvictorConfig struct {
	TTLDefaultHours  int           // default TTL for new chunks
	LRUCapBytes      int64         // max total bytes for all chunks (0 = unlimited)
	EvictionInterval time.Duration // how often background eviction runs
}

// Evictor runs periodic TTL expiration and LRU cap enforcement.
type Evictor struct {
	store  *Store
	db     *sql.DB
	cfg    EvictorConfig
	logger *slog.Logger

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// NewEvictor creates a new Evictor. If logger is nil, uses slog.Default().
func NewEvictor(store *Store, db *sql.DB, cfg EvictorConfig, logger *slog.Logger) *Evictor {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.EvictionInterval < minEvictionInterval {
		cfg.EvictionInterval = minEvictionInterval
	}
	return &Evictor{
		store:  store,
		db:     db,
		cfg:    cfg,
		logger: logger,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// RunEviction performs one eviction cycle: TTL expiration then LRU cap enforcement.
// Returns the total number of chunks deleted.
func (e *Evictor) RunEviction(ctx context.Context) (int, error) {
	var totalDeleted int

	expiredIDs, err := e.store.GetExpiredChunkIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("get expired chunks: %w", err)
	}
	if len(expiredIDs) > 0 {
		if err := e.store.DeleteChunks(ctx, expiredIDs); err != nil {
			return 0, fmt.Errorf("delete expired chunks: %w", err)
		}
		totalDeleted += len(expiredIDs)
		e.logger.Info("evicted expired chunks", "count", len(expiredIDs))
	}

	if e.cfg.LRUCapBytes > 0 {
		totalSize, err := e.store.GetTotalSize(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("get total size: %w", err)
		}
		if totalSize > e.cfg.LRUCapBytes {
			deleted, err := e.evictLRU(ctx, totalSize)
			if err != nil {
				return totalDeleted, fmt.Errorf("lru eviction: %w", err)
			}
			totalDeleted += deleted
		}
	}

	return totalDeleted, nil
}

// evictLRU deletes oldest chunks until total size is at or below the cap.
func (e *Evictor) evictLRU(ctx context.Context, currentSize int64) (int, error) {
	rows, err := e.db.QueryContext(ctx,
		`SELECT id, LENGTH(content) FROM content_chunks ORDER BY indexed_at ASC`)
	if err != nil {
		return 0, fmt.Errorf("query oldest chunks: %w", err)
	}

	var toDelete []string
	freed := int64(0)
	target := currentSize - e.cfg.LRUCapBytes

	for rows.Next() {
		if freed >= target {
			break
		}
		var id string
		var size int
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan chunk for lru: %w", err)
		}
		toDelete = append(toDelete, id)
		freed += int64(size)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close lru rows: %w", err)
	}

	if len(toDelete) > 0 {
		if err := e.store.DeleteChunks(ctx, toDelete); err != nil {
			return 0, fmt.Errorf("delete lru chunks: %w", err)
		}
		e.logger.Info("evicted lru chunks", "count", len(toDelete), "freed_bytes", freed)
	}

	return len(toDelete), nil
}

// Start launches the background eviction goroutine.
func (e *Evictor) Start(ctx context.Context) {
	go func() {
		defer close(e.doneCh)

		ticker := time.NewTicker(e.cfg.EvictionInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				e.logger.Info("evictor stopping: context cancelled")
				return
			case <-e.stopCh:
				e.logger.Info("evictor stopping: closed")
				return
			case <-ticker.C:
				deleted, err := e.RunEviction(ctx)
				if err != nil {
					e.logger.Error("eviction cycle failed", "error", err)
				} else if deleted > 0 {
					e.logger.Info("eviction cycle completed", "deleted", deleted)
				}
			}
		}
	}()
}

// Close signals the background goroutine to stop and waits for it to finish.
// Safe to call multiple times.
func (e *Evictor) Close() {
	e.once.Do(func() {
		close(e.stopCh)
		select {
		case <-e.doneCh:
		case <-time.After(5 * time.Second):
			e.logger.Warn("evictor close timed out")
		}
	})
}
