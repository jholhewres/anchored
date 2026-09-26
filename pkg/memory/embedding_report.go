package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// GenerationReport describes an active or building embedding generation for
// `anchored doctor` and `anchored stats`, computed from the database alone
// (no model is loaded).
type GenerationReport struct {
	ID            string
	State         EmbeddingGenerationState
	ModelRevision string
	Vectors       int     // live memories with a current vector in it
	Live          int     // live memories
	Health        float64 // mean cosine of sampled pairs; NaN below two vectors
}

// Coverage is Vectors/Live as a percentage (100 when there is nothing to
// embed).
func (g GenerationReport) Coverage() float64 {
	if g.Live == 0 {
		return 100
	}
	return 100 * float64(g.Vectors) / float64(g.Live)
}

// ReportEmbeddingGenerations reports every active or building generation,
// the active one first, sampling up to sample vectors of each for health.
func ReportEmbeddingGenerations(ctx context.Context, db *sql.DB, sample int) ([]GenerationReport, error) {
	var live int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories m
		JOIN memory_revisions r ON r.revision_id = m.current_revision_id
		WHERE m.deleted_at IS NULL AND r.is_tombstone = FALSE`).Scan(&live); err != nil {
		return nil, fmt.Errorf("count live memories: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT generation_id, state, model_revision
		FROM embedding_generations WHERE state IN ('active', 'building')
		ORDER BY state = 'active' DESC, created_at`)
	if err != nil {
		return nil, fmt.Errorf("list embedding generations: %w", err)
	}
	var out []GenerationReport
	for rows.Next() {
		var g GenerationReport
		if err := rows.Scan(&g.ID, &g.State, &g.ModelRevision); err != nil {
			rows.Close()
			return nil, err
		}
		g.Live = live
		out = append(out, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*)
			FROM memories m
			JOIN memory_revisions r ON r.revision_id = m.current_revision_id
			JOIN memory_embedding_vectors v
			  ON v.revision_id = r.revision_id AND v.generation_id = ?
			 AND v.purpose = 'document' AND v.content_hash = r.content_hash
			WHERE m.deleted_at IS NULL AND r.is_tombstone = FALSE`, out[i].ID).Scan(&out[i].Vectors); err != nil {
			return nil, fmt.Errorf("count generation vectors: %w", err)
		}
		vecs, err := sampleGenerationVectors(ctx, db, out[i].ID, sample)
		if err != nil {
			return nil, err
		}
		out[i].Health = math.NaN()
		if len(vecs) >= 2 {
			out[i].Health = MeanPairwiseCosine(vecs)
		}
	}
	return out, nil
}

func sampleGenerationVectors(ctx context.Context, db *sql.DB, generationID string, n int) ([][]float32, error) {
	rows, err := db.QueryContext(ctx, `SELECT v.embedding
		FROM memory_embedding_vectors v
		JOIN memories m ON m.current_revision_id = v.revision_id AND m.deleted_at IS NULL
		WHERE v.generation_id = ? AND v.purpose = 'document'
		ORDER BY random() LIMIT ?`, generationID, n)
	if err != nil {
		return nil, fmt.Errorf("sample embedding vectors: %w", err)
	}
	defer rows.Close()
	var out [][]float32
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, err
		}
		vec, err := blobToFloat32s(blob)
		if err != nil {
			return nil, err
		}
		out = append(out, vec)
	}
	return out, rows.Err()
}
