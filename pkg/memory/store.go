package memory

import (
	"context"
	"database/sql"
	"time"
)

type Memory struct {
	ID           string     `json:"id"`
	ProjectID    *string    `json:"project_id,omitempty"`
	Category     string     `json:"category"`
	Content      string     `json:"content"`
	ContentHash  string     `json:"content_hash,omitempty"`
	Keywords     []string   `json:"keywords,omitempty"`
	Embedding    []float32  `json:"embedding,omitempty"`
	Source       string     `json:"source"`
	SourceID     *string    `json:"source_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	AccessCount  int        `json:"access_count"`
	LastAccessed *time.Time `json:"last_accessed,omitempty"`
	Metadata     any        `json:"metadata,omitempty"`
}

type SearchResult struct {
	Memory Memory
	Score  float64
}

type SearchOptions struct {
	MaxResults     int
	Category       string
	ProjectID      string
	BoostProjectID string // Project to boost in results (separate from filter)
	Since          *time.Time
}

type ListOptions struct {
	Limit     int
	Offset    int
	Category  string
	ProjectID string
}

type StoreStats struct {
	TotalMemories int            `json:"total_memories"`
	ByCategory    map[string]int `json:"by_category"`
	ByProject     map[string]int `json:"by_project"`
}

type SaveOptions struct {
	Content   string
	Category  string
	Source    string
	SourceID  *string
	CWD       string
	SkipEmbed bool
}

type DeleteScopeOptions struct {
	ProjectID string
	Category  string
	Source    string
	Hard      bool
}

type Store interface {
	Save(ctx context.Context, m Memory) error
	Get(ctx context.Context, id string) (*Memory, error)
	Update(ctx context.Context, id, content, category string) error
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
	Delete(ctx context.Context, id string) error
	SoftDelete(ctx context.Context, id string) error
	DeleteByScope(ctx context.Context, opts DeleteScopeOptions) (int, error)
	List(ctx context.Context, opts ListOptions) ([]Memory, error)
	Stats(ctx context.Context) (*StoreStats, error)
	UpdateEmbedding(ctx context.Context, id string, embedding []float32) error
	ListWithoutEmbedding(ctx context.Context, limit int) ([]Memory, error)
	FindByContentHash(ctx context.Context, hash string, projectID *string) (*Memory, error)
	BackfillContentHash(ctx context.Context) (int, error)
	DB() *sql.DB
	VectorCache() *VectorCache
	Close() error
}
