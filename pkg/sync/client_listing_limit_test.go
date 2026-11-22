package sync

import (
	"context"
	"errors"
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

// endlessServer answers every request with status and a body that never
// ends, counting the bytes the client takes.
func endlessServer(t *testing.T, status int, written *atomic.Int64) *httptest.Server {
	t.Helper()
	chunk := []byte(`{"padding":"padding-padding-padding-padding-padding"},`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		for written.Load() < 64<<20 {
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRemoteCallsCapTheBodiesTheyReadWhole locks the cap on every JSON call
// that reads a remote's answer whole: an endless success fails the call
// after a bounded read, and an endless error body still comes back as the
// remote's status, so a refusal is not mistaken for a transport failure.
func TestRemoteCallsCapTheBodiesTheyReadWhole(t *testing.T) {
	calls := map[string]func(*Client) error{
		"save": func(c *Client) error {
			_, err := c.SaveRemote(context.Background(), RemoteMemory{ID: "m1", Category: "fact", Content: "x", ProjectID: "p1"})
			return err
		},
		"search": func(c *Client) error {
			_, err := c.SearchRemote(context.Background(), "p1", "query", 5)
			return err
		},
		"triples": func(c *Client) error {
			_, err := c.PushTriples(context.Background(), "p1", []SyncTriple{{Subject: "a", Predicate: "uses", Object: "b"}})
			return err
		},
		"projects": func(c *Client) error {
			_, err := c.ListProjects(context.Background())
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+" success", func(t *testing.T) {
			var written atomic.Int64
			srv := endlessServer(t, http.StatusOK, &written)
			c := NewClientFromEntry(config.RemoteEntry{Name: "evil", ServerURL: srv.URL, APIKey: "k1"}, "test")
			if err := call(c); err == nil {
				t.Fatal("an endless body decoded")
			}
			srv.Close()
			if got := written.Load(); got >= 32<<20 {
				t.Fatalf("the client took %d bytes, want it to stop near the %d-byte cap", got, maxRemoteResponseBytes)
			}
		})
		t.Run(name+" refusal", func(t *testing.T) {
			var written atomic.Int64
			srv := endlessServer(t, http.StatusUnauthorized, &written)
			c := NewClientFromEntry(config.RemoteEntry{Name: "evil", ServerURL: srv.URL, APIKey: "k1"}, "test")
			err := call(c)
			var remoteErr *RemoteError
			if !errors.As(err, &remoteErr) || remoteErr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("an endless 401 came back as %v, want the remote's 401", err)
			}
			if len(remoteErr.Body) > maxRemoteErrorBodyBytes {
				t.Fatalf("kept %d bytes of the error body, want at most %d", len(remoteErr.Body), maxRemoteErrorBodyBytes)
			}
		})
	}
}
