package sync

type Memory struct {
	ID               string  `json:"id"`
	Category         string  `json:"category"`
	Content          string  `json:"content"`
	ProjectID        *string `json:"project_id,omitempty"`
	Source           string  `json:"source"`
	SyncOrigin       string  `json:"sync_origin"`
	SyncDirty        bool    `json:"sync_dirty"`
	RemoteProjectKey *string `json:"remote_project_key,omitempty"`
	PreferenceScope  string  `json:"preference_scope,omitempty"`
	Metadata         any     `json:"metadata,omitempty"`
}

type PreviewClassification string

const (
	ClassificationSyncable    PreviewClassification = "syncable"
	ClassificationBlocked     PreviewClassification = "blocked"
	ClassificationNeedsReview PreviewClassification = "needs_review"
)

type PreviewItem struct {
	Memory         Memory                `json:"memory"`
	Classification PreviewClassification `json:"classification"`
	Reason         string                `json:"reason"`
}

type PreviewResult struct {
	Total       int           `json:"total"`
	Syncable    int           `json:"syncable"`
	Blocked     int           `json:"blocked"`
	NeedsReview int           `json:"needs_review"`
	Items       []PreviewItem `json:"items,omitempty"`
}
