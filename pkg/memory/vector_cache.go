package memory

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"runtime"
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
	byID  map[string][]float32  // exact vectors (Get/All contract preserved)
	quant map[string]quantEntry // memoized quantized form + norm for scoring
	// scope is the project of each memory, "" for a global one, so a scoped
	// search takes its top-k inside the scope. It follows memories, not
	// vectors: Replace keeps it, the store refreshes it when it loads a
	// generation and on every vector write. An id missing here is unknown and
	// is scored in every scope, and the caller's per-memory filter has the
	// final word. A stale entry (a memory another process moved since, or a
	// bulk refresh racing a single write) can still keep a memory out of the
	// top-k of its new project until the next refresh.
	scope  map[string]string
	mu     sync.RWMutex
	logger *slog.Logger

	// warmMu guards warmCh, the handshake for an asynchronous fill. Serving a
	// large corpus, the fill costs seconds (decode + quantize every vector), so
	// the server hands it to a goroutine instead of blocking startup. warmCh is
	// non-nil while a fill is in flight and is closed when it finishes, letting
	// the first search wait for real vectors rather than silently scoring
	// against an empty cache.
	warmMu sync.Mutex
	warmCh chan struct{}
}

func NewVectorCache(logger *slog.Logger) *VectorCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &VectorCache{
		byID:   make(map[string][]float32),
		quant:  make(map[string]quantEntry),
		scope:  make(map[string]string),
		logger: logger,
	}
}

// BeginWarm marks an asynchronous fill as in flight and returns the function
// that ends it. Calling it twice without finishing the first reuses the
// existing handshake, so concurrent warms collapse into one wait.
func (c *VectorCache) BeginWarm() func() {
	c.warmMu.Lock()
	if c.warmCh != nil {
		ch := c.warmCh
		c.warmMu.Unlock()
		return func() { c.finishWarm(ch) }
	}
	ch := make(chan struct{})
	c.warmCh = ch
	c.warmMu.Unlock()
	return func() { c.finishWarm(ch) }
}

func (c *VectorCache) finishWarm(ch chan struct{}) {
	c.warmMu.Lock()
	if c.warmCh == ch {
		c.warmCh = nil
		close(ch)
	}
	c.warmMu.Unlock()
}

// WaitWarm blocks until an in-flight fill finishes, ctx is done, or there is no
// fill to wait for. It reports whether the cache is settled: false means the
// caller gave up early and should treat the cache as incomplete.
func (c *VectorCache) WaitWarm(ctx context.Context) bool {
	c.warmMu.Lock()
	ch := c.warmCh
	c.warmMu.Unlock()
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
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
	return c.ScoreInScope(query, queryNorm, minScore, topK, "")
}

// ScoreInScope returns the topK entries scoring above minScore, best first and
// ties broken by id, so the same query over the same cache always ranks the
// same way. With a project, only that project's memories, the global ones and
// those of unknown scope compete; "" scores everything.
func (c *VectorCache) ScoreInScope(query []float32, queryNorm, minScore float64, topK int, project string) []ScoredID {
	var h scoredHeap
	c.mu.RLock()
	for id, e := range c.quant {
		if project != "" {
			if sc, known := c.scope[id]; known && sc != "" && sc != project {
				continue
			}
		}
		s := e.q.CosineWithNorm(query, queryNorm, e.norm)
		if s <= minScore {
			continue
		}
		item := ScoredID{ID: id, Score: s}
		switch {
		case topK <= 0 || h.Len() < topK:
			heap.Push(&h, item)
		case scoredBefore(item, h[0]):
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	c.mu.RUnlock()

	out := []ScoredID(h)
	sort.Slice(out, func(i, j int) bool { return scoredBefore(out[i], out[j]) })
	return out
}

// scoredBefore orders by score, then id: the ranking of equal scores must not
// depend on map iteration.
func scoredBefore(a, b ScoredID) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.ID < b.ID
}

// scoredHeap is a min-heap on scoredBefore: its root is the weakest of the
// top-k kept so far.
type scoredHeap []ScoredID

func (h scoredHeap) Len() int           { return len(h) }
func (h scoredHeap) Less(i, j int) bool { return scoredBefore(h[j], h[i]) }
func (h scoredHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *scoredHeap) Push(x any)        { *h = append(*h, x.(ScoredID)) }
func (h *scoredHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	*h = old[:len(old)-1]
	return item
}

// SetScope records the project of memory id ("" for a global memory).
func (c *VectorCache) SetScope(id, project string) {
	c.mu.Lock()
	c.scope[id] = project
	c.mu.Unlock()
}

// SetScopes replaces every recorded scope.
func (c *VectorCache) SetScopes(scopes map[string]string) {
	next := make(map[string]string, len(scopes))
	for id, p := range scopes {
		next[id] = p
	}
	c.mu.Lock()
	c.scope = next
	c.mu.Unlock()
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
	delete(c.scope, id)
	c.mu.Unlock()
}

// Replace atomically swaps the cache contents. Generation activation uses it
// so queries never observe a mixture of vectors from two semantic spaces.
//
// The copy+quantize pass is the expensive half of server startup on a large
// corpus (one QuantizeFloat32 plus a norm per vector), and each entry is
// independent, so it fans out across cores. The result is identical to the
// sequential form: workers write to disjoint slice slots and the maps are built
// once, in a single pass, afterwards.
func (c *VectorCache) Replace(vectors map[string][]float32) {
	n := len(vectors)
	ids := make([]string, 0, n)
	src := make([][]float32, 0, n)
	for id, vector := range vectors {
		ids = append(ids, id)
		src = append(src, vector)
	}

	copies := make([][]float32, n)
	entries := make([]quantEntry, n)
	parallelFor(n, func(i int) {
		cp := append([]float32(nil), src[i]...)
		copies[i] = cp
		entries[i] = makeQuantEntry(cp)
	})

	byID := make(map[string][]float32, n)
	quant := make(map[string]quantEntry, n)
	for i, id := range ids {
		byID[id] = copies[i]
		quant[id] = entries[i]
	}

	c.mu.Lock()
	c.byID = byID
	c.quant = quant
	c.mu.Unlock()
}

// parallelForMinBatch is the point below which goroutine setup costs more than
// the work it distributes; smaller runs stay on the calling goroutine.
const parallelForMinBatch = 512

// parallelFor applies fn to every index in [0, n) across GOMAXPROCS workers.
// fn must only touch index-local state.
func parallelFor(n int, fn func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if n < parallelForMinBatch || workers < 2 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	if workers > n {
		workers = n
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	chunk := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := start + chunk
		if end > n {
			end = n
		}
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				fn(i)
			}
		}(start, end)
	}
	wg.Wait()
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
