package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
)

type EmbeddingCache struct {
	db     *sql.DB
	logger *slog.Logger
}

func NewEmbeddingCache(db *sql.DB, logger *slog.Logger) *EmbeddingCache {
	if logger == nil {
		logger = slog.Default()
	}
	return &EmbeddingCache{db: db, logger: logger}
}

func (c *EmbeddingCache) Get(ctx context.Context, text, model string) ([]float32, bool) {
	key := hashText(text)

	var data []byte
	err := c.db.QueryRowContext(ctx,
		"SELECT embedding FROM embedding_cache WHERE text_hash = ? AND model = ?",
		key, model,
	).Scan(&data)
	if err != nil {
		return nil, false
	}

	var qe QuantizedEmbedding
	if err := qe.UnmarshalBinary(data); err != nil {
		c.logger.Warn("failed to unmarshal cached embedding", "error", err)
		return nil, false
	}

	vec := qe.Dequantize()
	return vec, true
}

func (c *EmbeddingCache) Put(ctx context.Context, text, model string, vec []float32, quantize bool) error {
	key := hashText(text)

	var data []byte
	if quantize {
		qe := QuantizeFloat32(vec)
		bin, err := qe.MarshalBinary()
		if err != nil {
			return fmt.Errorf("quantize embedding: %w", err)
		}
		data = bin
	} else {
		data = float32SliceToBytes(vec)
	}

	_, err := c.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO embedding_cache (text_hash, model, embedding) VALUES (?, ?, ?)`,
		key, model, data,
	)
	return err
}

func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func float32SliceToBytes(vec []float32) []byte {
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

const LegacyModelName = "all-MiniLM-L6-v2"

func (c *EmbeddingCache) MigrateFromLegacy(currentModel string) int64 {
	var count int64
	err := c.db.QueryRow(
		"SELECT COUNT(*) FROM embedding_cache WHERE model = ?",
		LegacyModelName,
	).Scan(&count)
	if err != nil || count == 0 {
		return 0
	}

	res, err := c.db.Exec(
		"DELETE FROM embedding_cache WHERE model = ?",
		LegacyModelName,
	)
	if err != nil {
		c.logger.Warn("failed to migrate legacy embedding cache", "error", err)
		return 0
	}
	deleted, _ := res.RowsAffected()
	c.logger.Info("Model updated. Re-generating embeddings in background...",
		"deleted_cache_entries", deleted,
		"old_model", LegacyModelName,
		"new_model", currentModel,
	)
	return deleted
}
