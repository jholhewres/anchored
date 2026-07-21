package memory

// Positional literals lock legacy exported shapes used by downstream callers.
var (
	_ = SearchOptions{0, "", "", "", nil, nil, false}
	_ = SaveOptions{"", "", "", nil, "", false, "", nil}
)
