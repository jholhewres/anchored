package memory

import (
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
)

// quantEntry memoizes the quantized form of a stored vector and its L2 norm so
// per-query scoring skips re-quantization and norm recomputation (both are
// invariant per stored vector). Computed once on Put/Load.
type quantEntry struct {
	q    QuantizedEmbedding
	norm float64 // sqrt(NormSq) in dequantized space
}

// ScoredID is one scored cache entry returned by Score.
type ScoredID struct {
	ID    string
	Score float64
}

// VectorCache is a thread-safe in-memory cache of memory embeddings keyed by memory ID.
type VectorCache struct {
	byID   map[string][]float32  // exact vectors (Get/All contract preserved)
	quant  map[string]quantEntry // memoized quantized form + norm for scoring
	mu     sync.RWMutex
	logger *slog.Logger
}

func NewVectorCache(logger *slog.Logger) *VectorCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &VectorCache{
		byID:   make(map[string][]float32),
		quant:  make(map[string]quantEntry),
		logger: logger,
	}
}

// makeQuantEntry quantizes a vector once and precomputes its norm.
func makeQuantEntry(vec []float32) quantEntry {
	q := QuantizeFloat32(vec)
	return quantEntry{q: q, norm: math.Sqrt(q.NormSq())}
}

// Score scans the cache once under a read lock — without copying the map —
// scoring every entry against the query via the memoized quantized form and
// norm, and returns the top-k entries scoring above minScore. This replaces a
// per-query All() copy + re-quantize + re-norm with a single allocation-light
// pass; scores are bit-identical to QuantizedEmbedding.CosineSimilarity.
func (c *VectorCache) Score(query []float32, queryNorm, minScore float64, topK int) []ScoredID {
	c.mu.RLock()
	scored := make([]ScoredID, 0, len(c.quant))
	for id, e := range c.quant {
		s := e.q.CosineWithNorm(query, queryNorm, e.norm)
		if s > minScore {
			scored = append(scored, ScoredID{ID: id, Score: s})
		}
	}
	c.mu.RUnlock()

	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if topK > 0 && len(scored) > topK {
		scored = scored[:topK]
	}
	return scored
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
		c.quant[id] = makeQuantEntry(vec)
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
	e := makeQuantEntry(cp)
	c.mu.Lock()
	c.byID[id] = cp
	c.quant[id] = e
	c.mu.Unlock()
}

func (c *VectorCache) Remove(id string) {
	c.mu.Lock()
	delete(c.byID, id)
	delete(c.quant, id)
	c.mu.Unlock()
}

// Replace atomically swaps the cache contents. Generation activation uses it
// so queries never observe a mixture of vectors from two semantic spaces.
func (c *VectorCache) Replace(vectors map[string][]float32) {
	byID := make(map[string][]float32, len(vectors))
	quant := make(map[string]quantEntry, len(vectors))
	for id, vector := range vectors {
		cp := append([]float32(nil), vector...)
		byID[id] = cp
		quant[id] = makeQuantEntry(cp)
	}
	c.mu.Lock()
	c.byID = byID
	c.quant = quant
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
