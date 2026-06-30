package memory

import (
	"context"
	"database/sql"
	"time"
)

type Memory struct {
	ID               string     `json:"id"`
	ProjectID        *string    `json:"project_id,omitempty"`
	Category         string     `json:"category"`
	Content          string     `json:"content"`
	ContentHash      string     `json:"content_hash,omitempty"`
	Keywords         []string   `json:"keywords,omitempty"`
	Embedding        []float32  `json:"embedding,omitempty"`
	Source           string     `json:"source"`
	SourceID         *string    `json:"source_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
	AccessCount      int        `json:"access_count"`
	LastAccessed     *time.Time `json:"last_accessed,omitempty"`
	Metadata         any        `json:"metadata,omitempty"`
	SyncDirty        bool       `json:"sync_dirty,omitempty"`
	SyncOrigin       string     `json:"sync_origin,omitempty"`
	Author           *string    `json:"author,omitempty"`
	RemoteProjectKey *string    `json:"remote_project_key,omitempty"`
}

type SearchResult struct {
	Memory Memory
	Score  float64
	// Signals explains why a result ranked where it did (e.g. "project",
	// "working_set", "pinned", "fresh", "low_signal"). Populated only when
	// SearchOptions.ExplainSignals is set; nil otherwise.
	Signals []string `json:"signals,omitempty"`
}

// WorkingSetSignals is the retrieval-relevant projection of a session working
// set: the files/symbols/entities currently in focus. Memories whose content or
// keywords overlap these get a working-set boost. Kept dependency-free here so
// pkg/memory never imports pkg/session.
type WorkingSetSignals struct {
	Files    []string
	Symbols  []string
	Entities []string
}

// Empty reports whether there are no focus tokens to boost on.
func (w *WorkingSetSignals) Empty() bool {
	return w == nil || len(w.Files)+len(w.Symbols)+len(w.Entities) == 0
}

type SearchOptions struct {
	MaxResults     int
	Category       string
	ProjectID      string
	BoostProjectID string // Project to boost in results (separate from filter)
	Since          *time.Time
	// WorkingSet, when set, boosts results overlapping the session's current
	// focus (files/symbols/entities). Nil disables the boost.
	WorkingSet *WorkingSetSignals
	// ExplainSignals populates SearchResult.Signals with ranking rationale.
	ExplainSignals bool
}

type ListOptions struct {
	Limit    int
	Offset   int
	Category string
	// Categories lets callers filter on multiple categories with one query.
	// When non-empty, takes precedence over Category. Use it for L0 context
	// builders that want decision/learning/plan/preference/fact in one shot.
	Categories     []string
	ProjectID      string
	Source         string
	IncludeDeleted bool
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
	// PreferenceScope is used only when Category is "preference".
	// Empty defaults to "user" so inferred preferences remain local/personal by default.
	PreferenceScope string
	// Metadata overrides the auto-generated metadata for this save.
	// When non-nil, takes precedence over WithPreferenceScope logic.
	// Callers should construct MemoryMetadata and call ToAny(), or use
	// the HandoffMetadata helper for handoff snapshots.
	Metadata any
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
	UpdateMetadata(ctx context.Context, id string, metadata any) error
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
	Delete(ctx context.Context, id string) error
	SoftDelete(ctx context.Context, id string) error
	Restore(ctx context.Context, id string) error
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

type MemoryObserver interface {
	OnMemorySaved(ctx context.Context, m Memory)
	OnMemoryUpdated(ctx context.Context, m Memory)
	OnMemoryDeleted(ctx context.Context, id string, projectID *string)
	OnMemoryRestored(ctx context.Context, id string, projectID *string)
}
