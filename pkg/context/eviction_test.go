package ctx

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

func newEvictorTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	db := openStoreTestDB(t)
	s := NewStore(db, nil)
	if err := s.PrepareStatements(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return s, db
}

func TestEvictor_TTLExpired(t *testing.T) {
	s, db := newEvictorTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	expired := &Chunk{
		Source: "execute", Content: "old data", ContentType: "code",
		IndexedAt: now.Add(-2 * time.Hour), TTLHours: 1,
	}
	valid := &Chunk{
		Source: "execute", Content: "fresh data", ContentType: "code",
		IndexedAt: now, TTLHours: 336,
	}
	if err := s.InsertChunk(ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if err := s.InsertChunk(ctx, valid); err != nil {
		t.Fatalf("insert valid: %v", err)
	}

	ev := NewEvictor(s, db, EvictorConfig{LRUCapBytes: 0}, nil)
	deleted, err := ev.RunEviction(ctx)
	if err != nil {
		t.Fatalf("run eviction: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted: got %d, want 1", deleted)
	}

	got, _ := s.GetChunk(ctx, expired.ID)
	if got != nil {
		t.Error("expired chunk should be deleted")
	}
	got, _ = s.GetChunk(ctx, valid.ID)
	if got == nil {
		t.Error("valid chunk should still exist")
	}
}

func TestEvictor_LRUCap(t *testing.T) {
	s, db := newEvictorTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	content := strings.Repeat("x", 50)
	threeChunks := []*Chunk{
		{Source: "execute", Content: content, ContentType: "code", IndexedAt: now.Add(-2 * time.Minute), TTLHours: 336},
		{Source: "execute", Content: content, ContentType: "code", IndexedAt: now.Add(-1 * time.Minute), TTLHours: 336},
		{Source: "execute", Content: content, ContentType: "code", IndexedAt: now, TTLHours: 336},
	}
	for _, c := range threeChunks {
		if err := s.InsertChunk(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	ev := NewEvictor(s, db, EvictorConfig{LRUCapBytes: 100}, nil)
	deleted, err := ev.RunEviction(ctx)
	if err != nil {
		t.Fatalf("run eviction: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted: got %d, want 1", deleted)
	}

	got, _ := s.GetChunk(ctx, threeChunks[0].ID)
	if got != nil {
		t.Error("oldest chunk should be evicted")
	}

	totalSize, _ := s.GetTotalSize(ctx)
	if totalSize > 100 {
		t.Errorf("total size %d exceeds cap 100", totalSize)
	}
}

func TestEvictor_CombinedTTLAndLRU(t *testing.T) {
	s, db := newEvictorTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	content := strings.Repeat("y", 60)
	chunks := []*Chunk{
		{Source: "execute", Content: "expired", ContentType: "code", IndexedAt: now.Add(-2 * time.Hour), TTLHours: 1},
		{Source: "execute", Content: content, ContentType: "code", IndexedAt: now.Add(-1 * time.Minute), TTLHours: 336},
		{Source: "execute", Content: content, ContentType: "code", IndexedAt: now, TTLHours: 336},
	}
	for _, c := range chunks {
		if err := s.InsertChunk(ctx, c); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	ev := NewEvictor(s, db, EvictorConfig{LRUCapBytes: 80}, nil)
	deleted, err := ev.RunEviction(ctx)
	if err != nil {
		t.Fatalf("run eviction: %v", err)
	}
	if deleted < 2 {
		t.Errorf("deleted: got %d, want at least 2 (1 TTL + 1 LRU)", deleted)
	}

	got, _ := s.GetChunk(ctx, chunks[0].ID)
	if got != nil {
		t.Error("expired chunk should be deleted")
	}

	totalSize, _ := s.GetTotalSize(ctx)
	if totalSize > 80 {
		t.Errorf("total size %d exceeds cap 80", totalSize)
	}
}

func TestEvictor_EmptyStore(t *testing.T) {
	s, db := newEvictorTestStore(t)
	ctx := context.Background()

	ev := NewEvictor(s, db, EvictorConfig{LRUCapBytes: 100}, nil)
	deleted, err := ev.RunEviction(ctx)
	if err != nil {
		t.Fatalf("run eviction on empty store: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted: got %d, want 0", deleted)
	}
}

func TestEvictor_StartStop(t *testing.T) {
	s, db := newEvictorTestStore(t)
	ctx := context.Background()

	ev := NewEvictor(s, db, EvictorConfig{
		EvictionInterval: time.Minute,
		LRUCapBytes:      0,
	}, nil)

	ev.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	ev.Close()

	now := time.Now().UTC()
	chunk := &Chunk{
		Source: "execute", Content: "survives", ContentType: "code",
		IndexedAt: now, TTLHours: 336,
	}
	if err := s.InsertChunk(ctx, chunk); err != nil {
		t.Fatalf("insert after close: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	got, _ := s.GetChunk(ctx, chunk.ID)
	if got == nil {
		t.Error("chunk inserted after close should not be evicted")
	}

	ev.Close()
}

func TestEvictor_MinIntervalEnforced(t *testing.T) {
	s, db := newEvictorTestStore(t)

	ev := NewEvictor(s, db, EvictorConfig{
		EvictionInterval: 5 * time.Second,
	}, nil)
	if ev.cfg.EvictionInterval < time.Minute {
		t.Errorf("expected minimum 1 minute, got %v", ev.cfg.EvictionInterval)
	}
}
