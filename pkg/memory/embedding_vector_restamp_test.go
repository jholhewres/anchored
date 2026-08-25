package memory

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// A vector row whose content_hash no longer matches its revision is stale. If
// PutEmbeddingVector skips it instead of overwriting, the stamp becomes
// permanent: the upsert reports success, writes nothing, and the generation can
// never satisfy the coverage its activation check demands — so the embedding
// worker re-embeds the same revision forever and semantic search stays off.
func TestPutEmbeddingVectorRestampsStaleRow(t *testing.T) {
	store, err := NewSQLiteStore(
		filepath.Join(t.TempDir(), "restamp.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	identity := EmbeddingIdentity{
		Provider: "onnx", Model: "m", ModelRevision: "r1",
		Dimensions: 3, Normalization: "l2",
	}
	spaceID := identity.SemanticSpaceID()

	const memID = "mem-restamp"
	if err := store.Save(ctx, Memory{
		ID:        memID,
		Category:  "fact",
		Content:   "conteudo que sera reembeddado",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	var revisionID, revisionHash string
	if err := store.db.QueryRowContext(ctx,
		"SELECT revision_id, content_hash FROM memory_revisions WHERE memory_id = ?", memID,
	).Scan(&revisionID, &revisionHash); err != nil {
		t.Fatalf("read revision: %v", err)
	}

	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO embedding_generations
		   (generation_id, semantic_space_id, provider, model, model_revision,
		    dimensions, normalization, state, snapshot_at, created_at)
		 VALUES ('gen-restamp', ?, 'onnx', 'm', 'r1', 3, 'l2',
		         'building', 0, 0)`, spaceID,
	); err != nil {
		t.Fatalf("seed generation: %v", err)
	}

	// A row written before the revision's hash was known — exactly the shape
	// found in production, where legacy revisions carried a NULL content_hash.
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO memory_embedding_vectors
		   (revision_id, memory_id, generation_id, semantic_space_id, purpose,
		    provider, model, model_revision, dimensions, normalization,
		    content_hash, embedding, embedded_at)
		 VALUES (?, ?, 'gen-restamp', ?, 'document', 'onnx', 'm',
		         'r1', 3, 'l2', '', X'00', 0)`,
		revisionID, memID, spaceID,
	); err != nil {
		t.Fatalf("seed stale vector: %v", err)
	}

	err = store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{
		RevisionID:      revisionID,
		MemoryID:        memID,
		GenerationID:    "gen-restamp",
		SemanticSpaceID: spaceID,
		Purpose:         EmbeddingPurposeDocument,
		Identity:        identity,
		ContentHash:     revisionHash,
		Vector:          []float32{0.1, 0.2, 0.3},
		EmbeddedAt:      time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("PutEmbeddingVector: %v", err)
	}

	var got string
	if err := store.db.QueryRowContext(ctx,
		`SELECT content_hash FROM memory_embedding_vectors
		 WHERE revision_id = ? AND generation_id = 'gen-restamp' AND purpose = 'document'`,
		revisionID,
	).Scan(&got); err != nil {
		t.Fatalf("read vector: %v", err)
	}
	if got != revisionHash {
		t.Fatalf("stale content_hash survived the upsert: got %q, want %q — "+
			"the write was silently skipped, which is what strands a generation "+
			"in 'building' forever", got, revisionHash)
	}

	// And the row must now satisfy the very predicate activation checks.
	var missing int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories m
		JOIN memory_revisions r ON r.revision_id = m.current_revision_id
		WHERE m.deleted_at IS NULL AND r.is_tombstone = FALSE
		  AND NOT EXISTS (
			SELECT 1 FROM memory_embedding_vectors v
			WHERE v.revision_id = r.revision_id
			  AND v.generation_id = 'gen-restamp'
			  AND v.purpose = 'document'
			  AND v.content_hash = r.content_hash)`,
	).Scan(&missing); err != nil {
		t.Fatalf("coverage check: %v", err)
	}
	if missing != 0 {
		t.Errorf("coverage still reports %d revision(s) missing after restamp", missing)
	}
}
