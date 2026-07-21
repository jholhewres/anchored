package sync

// Positional literals intentionally lock the exported legacy shapes that
// external callers may compile against.
var (
	_ = SaveRemoteResponse{"", "", "", false}
	_ = RemoteSearchResult{"", "", "", "", "", "", ""}
	_ = RemoteError{0, ""}
)
