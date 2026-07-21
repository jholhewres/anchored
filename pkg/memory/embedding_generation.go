package memory

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type EmbeddingGenerationState string

const (
	EmbeddingGenerationBuilding EmbeddingGenerationState = "building"
	EmbeddingGenerationActive   EmbeddingGenerationState = "active"
	EmbeddingGenerationRetired  EmbeddingGenerationState = "retired"
)

var (
	ErrEmbeddingGenerationIncomplete = errors.New("embedding generation coverage is incomplete")
	ErrEmbeddingGenerationTransition = errors.New("invalid embedding generation transition")
	ErrEmbeddingDimensionMismatch    = errors.New("embedding dimensions do not match generation")
)

type EmbeddingGeneration struct {
	ID              string                   `json:"id"`
	SemanticSpaceID string                   `json:"semantic_space_id"`
	Identity        EmbeddingIdentity        `json:"identity"`
	State           EmbeddingGenerationState `json:"state"`
	SnapshotAt      time.Time                `json:"snapshot_at"`
	CreatedAt       time.Time                `json:"created_at"`
	ActivatedAt     *time.Time               `json:"activated_at,omitempty"`
	RetiredAt       *time.Time               `json:"retired_at,omitempty"`
}

type EmbeddingVectorRecord struct {
	RevisionID      string            `json:"revision_id"`
	MemoryID        string            `json:"memory_id"`
	GenerationID    string            `json:"generation_id"`
	SemanticSpaceID string            `json:"semantic_space_id"`
	Purpose         EmbeddingPurpose  `json:"purpose"`
	Identity        EmbeddingIdentity `json:"identity"`
	ContentHash     string            `json:"content_hash"`
	Vector          []float32         `json:"-"`
	EmbeddedAt      time.Time         `json:"embedded_at"`
}

func (r EmbeddingVectorRecord) Validate() error {
	if r.RevisionID == "" || r.MemoryID == "" || r.GenerationID == "" {
		return fmt.Errorf("embedding vector: revision, memory, and generation are required")
	}
	if r.Purpose != EmbeddingPurposeDocument && r.Purpose != EmbeddingPurposeQuery {
		return fmt.Errorf("embedding vector: invalid purpose %q", r.Purpose)
	}
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if len(r.Vector) != r.Identity.Dimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrEmbeddingDimensionMismatch, len(r.Vector), r.Identity.Dimensions)
	}
	if r.SemanticSpaceID == "" {
		r.SemanticSpaceID = r.Identity.SemanticSpaceID()
	}
	if r.SemanticSpaceID != r.Identity.SemanticSpaceID() {
		return fmt.Errorf("embedding vector semantic space does not match identity")
	}
	return nil
}

// EmbeddingGenerationStore is additive. The legacy embedding column remains a
// compatibility projection; generation-aware search reads only the active
// compatible generation through this capability.
type EmbeddingGenerationStore interface {
	EnsureEmbeddingGeneration(ctx context.Context, identity EmbeddingIdentity) (*EmbeddingGeneration, error)
	ActiveEmbeddingGeneration(ctx context.Context) (*EmbeddingGeneration, error)
	EmbeddingGeneration(ctx context.Context, generationID string) (*EmbeddingGeneration, error)
	ListBuildingEmbeddingGenerations(ctx context.Context) ([]EmbeddingGeneration, error)
	PutEmbeddingVector(ctx context.Context, record EmbeddingVectorRecord) error
	ListMissingEmbeddingRevisions(ctx context.Context, generationID string, limit int) ([]MemoryRevision, error)
	CountMissingEmbeddingRevisions(ctx context.Context, generationID string) (int, error)
	EnsureEmbeddingGenerationJobs(ctx context.Context, generationID string, limit int) (int, error)
	ActivateEmbeddingGeneration(ctx context.Context, generationID string) error
	LoadEmbeddingGeneration(ctx context.Context, generationID string) (map[string][]float32, error)
}

// EmbeddingGenerationPublicationStore serializes the database generation
// transition, vector snapshot, and caller publication callback against vector
// writes. It prevents cache/identity publication from dropping a concurrent
// delta after the snapshot.
type EmbeddingGenerationPublicationStore interface {
	PublishEmbeddingGeneration(
		ctx context.Context,
		generationID string,
		activate bool,
		publish func(map[string][]float32) error,
	) error
}
