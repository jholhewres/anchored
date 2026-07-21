package memory

import (
	"errors"
	"testing"
)

func TestEmbeddingVectorRecordValidatesSemanticSpaceAndDimensions(t *testing.T) {
	identity := EmbeddingIdentity{
		Provider: "p", Model: "m", ModelRevision: "r", Dimensions: 2, Normalization: "l2",
	}
	base := EmbeddingVectorRecord{
		RevisionID: "r1", MemoryID: "m1", GenerationID: "g1",
		SemanticSpaceID: identity.SemanticSpaceID(),
		Purpose:         EmbeddingPurposeDocument, Identity: identity, Vector: []float32{1, 2},
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	wrongDims := base
	wrongDims.Vector = []float32{1}
	if err := wrongDims.Validate(); !errors.Is(err, ErrEmbeddingDimensionMismatch) {
		t.Fatalf("dimension error=%v", err)
	}
	wrongSpace := base
	wrongSpace.SemanticSpaceID = "different"
	if err := wrongSpace.Validate(); err == nil {
		t.Fatal("semantic-space mismatch should fail")
	}
}
