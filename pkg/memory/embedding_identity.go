package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// EmbeddingPurpose distinguishes vectors produced for indexed documents from
// vectors produced for queries. Providers may encode the two differently even
// when both vectors belong to the same comparable semantic space.
type EmbeddingPurpose string

const (
	EmbeddingPurposeDocument EmbeddingPurpose = "document"
	EmbeddingPurposeQuery    EmbeddingPurpose = "query"
)

// EmbeddingIdentity names a comparable semantic space. Dimensions alone are
// deliberately insufficient: two models with the same width can produce
// unrelated coordinate systems.
type EmbeddingIdentity struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ModelRevision string `json:"model_revision,omitempty"`
	Dimensions    int    `json:"dimensions"`
	Normalization string `json:"normalization"`
}

func (i EmbeddingIdentity) Validate() error {
	if strings.TrimSpace(i.Provider) == "" {
		return fmt.Errorf("embedding identity: provider is required")
	}
	if strings.TrimSpace(i.Model) == "" {
		return fmt.Errorf("embedding identity: model is required")
	}
	if i.Dimensions <= 0 {
		return fmt.Errorf("embedding identity: dimensions must be positive")
	}
	if strings.TrimSpace(i.Normalization) == "" {
		return fmt.Errorf("embedding identity: normalization is required")
	}
	return nil
}

// SemanticSpaceID is stable across processes and changes whenever any
// compatibility-defining identity component changes.
func (i EmbeddingIdentity) SemanticSpaceID() string {
	parts := []string{
		strings.TrimSpace(i.Provider),
		strings.TrimSpace(i.Model),
		strings.TrimSpace(i.ModelRevision),
		strconv.Itoa(i.Dimensions),
		strings.TrimSpace(i.Normalization),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "es_" + hex.EncodeToString(sum[:16])
}

func (i EmbeddingIdentity) Compatible(other EmbeddingIdentity) bool {
	return i.SemanticSpaceID() == other.SemanticSpaceID()
}

// PurposefulEmbeddingProvider is an optional extension. Existing providers
// remain source-compatible through EmbedForPurpose, which falls back to Embed.
type PurposefulEmbeddingProvider interface {
	EmbeddingProvider
	EmbedPurpose(ctx context.Context, purpose EmbeddingPurpose, texts []string) ([][]float32, error)
}

// RevisionedEmbeddingProvider is an optional identity extension for providers
// that can identify an immutable model revision.
type RevisionedEmbeddingProvider interface {
	EmbeddingProvider
	ModelRevision() string
}

// NormalizedEmbeddingProvider is an optional identity extension. Legacy
// providers are treated as L2-normalized, matching the existing contract.
type NormalizedEmbeddingProvider interface {
	EmbeddingProvider
	Normalization() string
}

func EmbeddingIdentityOf(provider EmbeddingProvider) (EmbeddingIdentity, error) {
	if provider == nil {
		return EmbeddingIdentity{}, fmt.Errorf("embedding identity: provider is nil")
	}
	identity := EmbeddingIdentity{
		Provider:      provider.Name(),
		Model:         provider.Model(),
		Dimensions:    provider.Dimensions(),
		Normalization: "l2",
	}
	if p, ok := provider.(RevisionedEmbeddingProvider); ok {
		identity.ModelRevision = p.ModelRevision()
	}
	if p, ok := provider.(NormalizedEmbeddingProvider); ok {
		identity.Normalization = p.Normalization()
	}
	if err := identity.Validate(); err != nil {
		return EmbeddingIdentity{}, err
	}
	return identity, nil
}

func EmbedForPurpose(ctx context.Context, provider EmbeddingProvider, purpose EmbeddingPurpose, texts []string) ([][]float32, error) {
	switch purpose {
	case EmbeddingPurposeDocument, EmbeddingPurposeQuery:
	default:
		return nil, fmt.Errorf("embedding purpose %q is invalid", purpose)
	}
	if p, ok := provider.(PurposefulEmbeddingProvider); ok {
		return p.EmbedPurpose(ctx, purpose, texts)
	}
	return provider.Embed(ctx, texts)
}
