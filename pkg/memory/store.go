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
	Keywords     []string   `json:"keywords,omitempty"`
	Source       string     `json:"source"`
	SourceID     *string    `json:"source_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	AccessCount  int        `json:"access_count"`
	LastAccessed *time.Time `json:"last_accessed,omitempty"`
	Metadata     any        `json:"metadata,omitempty"`
}

type SearchResult struct {
	Memory Memory
	Score  float64
}

type SearchOptions struct {
	MaxResults int
	Category   string
	ProjectID  string
	Since      *time.Time
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

type Store interface {
	Save(ctx context.Context, m Memory) error
	Get(ctx context.Context, id string) (*Memory, error)
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, opts ListOptions) ([]Memory, error)
	Stats(ctx context.Context) (*StoreStats, error)
	DB() *sql.DB
	Close() error
}
