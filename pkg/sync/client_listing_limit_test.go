package sync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

// TestListProjectsCapsTheListingARemoteSends locks the project listing the
// remote lookup reads (the call kg_add's write budget and the skill probe
// bound) to a ceiling: a remote that streams a huge listing must not make the
// client buffer all of it. It counts the bytes the remote got to hand over,
// never the time it took.
func TestListProjectsCapsTheListingARemoteSends(t *testing.T) {
	const offered = 64 << 20
	const ceiling = 32 << 20
	chunk := []byte(`{"id":"p","remote_key":"padding-padding-padding-padding"},`)
	var written atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte("[")); err != nil {
			return
		}
		for written.Load() < offered {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
		// No closing bracket: the listing never decodes, so the test costs
		// the buffered bytes only, not a million decoded projects.
	}))
	t.Cleanup(srv.Close)

	c := NewClientFromEntry(config.RemoteEntry{Name: "evil", ServerURL: srv.URL, APIKey: "k1"}, "test")
	if _, _, err := c.lookupProjectIDByRemoteKeys(context.Background(), "abc123key"); err == nil {
		t.Fatal("a listing that never closes decoded")
	}
	srv.Close()
	if got := written.Load(); got >= ceiling {
		t.Fatalf("the client took %d bytes of project listing from one remote, want it to stop under %d", got, ceiling)
	}
}
