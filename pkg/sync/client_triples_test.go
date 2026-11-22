package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

// TestPushTriples_SendsOnlyWhatTheSafetyFilterAllows locks the filter inside
// PushTriples, the one call every path that sends triples goes through: the
// server never sees a triple the filter holds back, a push of nothing but
// those makes no request, and the response counts what was kept local.
func TestPushTriples_SendsOnlyWhatTheSafetyFilterAllows(t *testing.T) {
	var requests atomic.Int32
	var received []SyncTriple
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req SyncTripleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, req.Triples...)
		if err := json.NewEncoder(w).Encode(SyncTripleResponse{Accepted: len(req.Triples)}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientFromEntry(config.RemoteEntry{Name: "team", ServerURL: srv.URL, APIKey: "k1"}, "test")
	unsafe := SyncTriple{Subject: "staging-db", Predicate: "connects_with", Object: "postgres://app:not-a-real-pass@10.0.0.5/app via /home/someone/secrets/db.env"}
	clean := SyncTriple{Subject: "api", Predicate: "uses", Object: "postgres"}

	resp, err := c.PushTriples(context.Background(), "p1", "/home/dev/proj", []SyncTriple{unsafe, clean})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted != 1 || resp.Withheld != 1 {
		t.Fatalf("response %+v, want 1 accepted and 1 withheld", resp)
	}
	if len(received) != 1 || received[0] != clean {
		t.Fatalf("the server received %+v, want only the clean triple", received)
	}

	resp, err = c.PushTriples(context.Background(), "p1", "/home/dev/proj", []SyncTriple{unsafe})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Withheld != 1 || requests.Load() != 1 {
		t.Fatalf("a push of only unsafe triples: response %+v after %d requests, want 1 withheld and no new request", resp, requests.Load())
	}
}
