package ctx

import "time"

type Chunk struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	Source      string    `json:"source"`       // 'execute', 'fetch', 'batch', 'index'
	Label       string    `json:"label"`
	Content     string    `json:"content"`
	Metadata    string    `json:"metadata,omitempty"`
	ContentType string    `json:"content_type,omitempty"` // 'code', 'prose'
	IndexedAt   time.Time `json:"indexed_at"`
	TTLHours    int       `json:"ttl_hours"`
}

type ContentSearchResult struct {
	ChunkID string  `json:"chunk_id"`
	Label   string  `json:"label"`
	Source  string  `json:"source"`
	Snippet string  `json:"snippet"`
	Score   float64 `json:"score"`
}

type ExecuteResult struct {
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr,omitempty"`
	ExitCode  int           `json:"exit_code"`
	Duration  time.Duration `json:"duration"`
	TimedOut  bool          `json:"timed_out"`
	Truncated bool          `json:"truncated"`
}

type BatchCommand struct {
	Label    string `json:"label"`
	Command  string `json:"command"`
	Language string `json:"language"`
}

type BatchResult struct {
	Results       []ExecuteResult       `json:"results"`
	SearchResults []ContentSearchResult `json:"search_results,omitempty"`
	SourceID      string                `json:"source_id"`
	TotalBytes    int64                 `json:"total_bytes"`
}

type FetchResult struct {
	Markdown    string    `json:"markdown"`
	ContentType string    `json:"content_type"`
	URL         string    `json:"url"`
	FetchedAt   time.Time `json:"fetched_at"`
	FromCache   bool      `json:"from_cache"`
}

type SessionEvent struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	EventType string    `json:"event_type"`
	Priority  int       `json:"priority"` // 1=critical, 2=high, 3=normal, 4=low
	ToolName  string    `json:"tool_name,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	Metadata  string    `json:"metadata,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type SearchOpts struct {
	MaxResults  int    `json:"max_results"`
	ContentType string `json:"content_type,omitempty"` // 'code', 'prose'
	Source      string `json:"source,omitempty"`       // filter by source label (partial match)
}
