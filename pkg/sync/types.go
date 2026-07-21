package sync

// SyncPushRequest is the payload sent to anchored_oss for pushing local memories.
// ProjectRoot is a client-side hint used by the local safety filter to rewrite
// in-project paths to relative form. It is intentionally not serialized.
type SyncPushRequest struct {
	ClientID  string `json:"client_id"`
	ProjectID string `json:"project_id"`
	// ProjectClaim lets the client route a push to a remote project by its
	// git-origin remote_key instead of a server-side project_id. When set and
	// ProjectID is empty, the server resolves-or-creates the project by
	// remote_key, using Name for a human-readable label. This is how
	// repo-scoped sync identifies a repository regardless of its local
	// directory name.
	ProjectClaim *ProjectClaim `json:"project_claim,omitempty"`
	Memories     []SyncMemory  `json:"memories"`
	ProjectRoot  string        `json:"-"`
	// ClientCapabilities advertises optional protocol features and, by its
	// presence, opts into the server's Policy hints. Omitted -> the server
	// responds exactly as it did before negotiation existed.
	ClientCapabilities *ClientCapabilities `json:"client_capabilities,omitempty"`
}

// ClientCapabilities mirrors the server's negotiation struct. This wave only
// the (empty) presence matters — it opts into policy hints; the feature flags
// stay false until later waves implement them.
type ClientCapabilities struct {
	PromotionQueue bool `json:"promotion_queue,omitempty"`
	TeamCache      bool `json:"team_cache,omitempty"`
	// ArtifactSummaries is reserved for a later wave (artifact-summary sync);
	// it stays false here so the server never emits the response plumbing,
	// while the field reserves the wire name for forward compatibility.
	ArtifactSummaries bool `json:"artifact_summaries,omitempty"`
}

// PolicyHints is the server's advisory sync policy, returned only when the
// request advertised ClientCapabilities. nil from older servers.
type PolicyHints struct {
	QualityThreshold   float64  `json:"quality_threshold"`
	BlockedCategories  []string `json:"blocked_categories"`
	MaxMemoriesPerSync int      `json:"max_memories_per_sync"`
}

// SyncMemory is a memory ready for remote sync (no embeddings, no local paths).
type SyncMemory struct {
	ID               string `json:"id"`
	Category         string `json:"category"`
	Content          string `json:"content"`
	Source           string `json:"source"`
	PreferenceScope  string `json:"preference_scope,omitempty"`
	RemoteProjectKey string `json:"remote_project_key,omitempty"`
	Metadata         any    `json:"metadata,omitempty"`
}

// SyncPushResponse is the server's response to a push request.
type SyncPushResponse struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors,omitempty"`
	// ProjectID is the remote project the batch landed in, resolved by the
	// server (servers >= v0.4.4). Needed for follow-up per-project calls
	// (e.g. PushTriples) when routing by project_claim, where the client
	// doesn't know the remote ID upfront. Empty on older servers.
	ProjectID string `json:"project_id,omitempty"`
	// Policy is the server's advisory sync policy, present only when the
	// request advertised capabilities and the server supports negotiation
	// (>= v0.5.1). nil otherwise.
	Policy *PolicyHints `json:"policy,omitempty"`
}

// SyncPullRequest requests new/updated memories from the server.
type SyncPullRequest struct {
	ClientID  string `json:"client_id"`
	ProjectID string `json:"project_id"`
	Watermark string `json:"watermark,omitempty"`
}

// SyncPullResponse is the server's response with remote memories.
type SyncPullResponse struct {
	Memories  []SyncMemory `json:"memories"`
	Watermark string       `json:"watermark"`
}

type RemoteMemory struct {
	ID           string        `json:"id"`
	Category     string        `json:"category"`
	Content      string        `json:"content"`
	Source       string        `json:"source,omitempty"`
	ProjectID    string        `json:"project_id,omitempty"`
	ProjectClaim *ProjectClaim `json:"project_claim,omitempty"`
}

type ProjectClaim struct {
	Name      string `json:"name"`
	RemoteKey string `json:"remote_key"`
}

type SaveRemoteResponse struct {
	ID        string `json:"id"`
	Category  string `json:"category"`
	ProjectID string `json:"project_id,omitempty"`
	Created   bool   `json:"created"`
}

// SyncTripleRequest pushes a batch of knowledge-graph triples to the remote
// server. Triples are scoped to a project; the caller must have resolved the
// remote project ID (e.g. via a prior memory sync) before calling.
type SyncTripleRequest struct {
	Triples []SyncTriple `json:"triples"`
}

type SyncTriple struct {
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Object     string  `json:"object"`
	Confidence float64 `json:"confidence,omitempty"`
}

type SyncTripleResponse struct {
	Accepted int      `json:"accepted"`
	Rejected int      `json:"rejected"`
	Errors   []string `json:"errors,omitempty"`
}

type RemoteSearchResult struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Content    string `json:"content"`
	ProjectID  string `json:"project_id"`
	Source     string `json:"source,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// RemoteSearchHit adds optional ranking metadata without changing the legacy
// RemoteSearchResult returned by SearchRemote. Older servers may omit all
// metadata fields.
type RemoteSearchHit struct {
	ID            string  `json:"id"`
	Category      string  `json:"category"`
	Content       string  `json:"content"`
	ProjectID     string  `json:"project_id"`
	Source        string  `json:"source,omitempty"`
	AuthorName    string  `json:"author_name,omitempty"`
	UpdatedAt     string  `json:"updated_at"`
	Rank          int     `json:"rank,omitempty"`
	Score         float64 `json:"score,omitempty"`
	EffectiveMode string  `json:"effective_mode,omitempty"`
	Origin        string  `json:"origin,omitempty"`
}

// RemoteSearchResponse exposes protocol metadata that cannot be represented
// on an empty top-level result array. SearchRemote remains the compatibility
// API for callers that only need hits.
type RemoteSearchResponse struct {
	Results        []RemoteSearchHit `json:"results"`
	RequestedMode  string            `json:"requested_mode"`
	EffectiveMode  string            `json:"effective_mode"`
	Fallback       bool              `json:"fallback"`
	FallbackReason string            `json:"fallback_reason,omitempty"`
}
