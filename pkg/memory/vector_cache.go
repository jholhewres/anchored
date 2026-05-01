package memory

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sync"
)

// VectorCache is a thread-safe in-memory cache of memory embeddings keyed by memory ID.
type VectorCache struct {
	byID   map[string][]float32
	mu     sync.RWMutex
	logger *slog.Logger
}

func NewVectorCache(logger *slog.Logger) *VectorCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &VectorCache{
		byID:   make(map[string][]float32),
		logger: logger,
	}
}

func (c *VectorCache) Load(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, embedding FROM memories WHERE embedding IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("vector cache load: %w", err)
	}
	defer rows.Close()

	loaded := 0
	c.mu.Lock()
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			c.mu.Unlock()
			return fmt.Errorf("vector cache load scan: %w", err)
		}
		vec, err := blobToFloat32s(data)
		if err != nil {
			c.logger.Warn("vector cache: skipping invalid embedding", "id", id, "error", err)
			continue
		}
		c.byID[id] = vec
		loaded++
	}
	c.mu.Unlock()

	if err := rows.Err(); err != nil {
		return fmt.Errorf("vector cache load rows: %w", err)
	}

	c.logger.Info("vector cache loaded", "count", loaded)
	return nil
}

func (c *VectorCache) Put(id string, embedding []float32) {
	cp := make([]float32, len(embedding))
	copy(cp, embedding)
	c.mu.Lock()
	c.byID[id] = cp
	c.mu.Unlock()
}

func (c *VectorCache) Remove(id string) {
	c.mu.Lock()
	delete(c.byID, id)
	c.mu.Unlock()
}

func (c *VectorCache) Get(id string) ([]float32, bool) {
	c.mu.RLock()
	vec, ok := c.byID[id]
	c.mu.RUnlock()
	return vec, ok
}

func (c *VectorCache) All() map[string][]float32 {
	c.mu.RLock()
	cp := make(map[string][]float32, len(c.byID))
	for k, v := range c.byID {
		cp[k] = v
	}
	c.mu.RUnlock()
	return cp
}

func (c *VectorCache) Len() int {
	c.mu.RLock()
	n := len(c.byID)
	c.mu.RUnlock()
	return n
}

func blobToFloat32s(data []byte) ([]float32, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d is not a multiple of 4", len(data))
	}
	n := len(data) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		bits := uint32(data[i*4]) | uint32(data[i*4+1])<<8 | uint32(data[i*4+2])<<16 | uint32(data[i*4+3])<<24
		vec[i] = math.Float32frombits(bits)
	}
	return vec, nil
}

func float32sToBlob(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		bits := math.Float32bits(v)
		buf[i*4] = byte(bits)
		buf[i*4+1] = byte(bits >> 8)
		buf[i*4+2] = byte(bits >> 16)
		buf[i*4+3] = byte(bits >> 24)
	}
	return buf
}
