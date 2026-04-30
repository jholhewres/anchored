package memory

import "context"

type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	Name() string
	Model() string
	Close() error
}
