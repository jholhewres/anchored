package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSearchRemote_SemanticByDefault(t *testing.T) {
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]RemoteSearchResult{{
			ID:        "m1",
			Category:  "fact",
			Content:   "semantic result",
			ProjectID: "project/with space",
			UpdatedAt: "2026-07-20T00:00:00Z",
		}})
	}))
	defer srv.Close()

	client := testSearchClient(srv)
	results, err := client.SearchRemote(context.Background(), "project/with space", "query + terms", 7)
	if err != nil {
		t.Fatalf("SearchRemote: %v", err)
	}
	if len(results) != 1 || results[0].ID != "m1" {
		t.Fatalf("unexpected results: %+v", results)
	}

	wantQuery := map[string][]string{
		"project_id": {"project/with space"},
		"q":          {"query + terms"},
		"limit":      {"7"},
		"mode":       {"semantic"},
	}
	if !reflect.DeepEqual(gotQuery, wantQuery) {
		t.Fatalf("query mismatch:\n got: %#v\nwant: %#v", gotQuery, wantQuery)
	}
}

func TestSearchRemote_SemanticUnavailableRetriesTextExactlyOnce(t *testing.T) {
	var modes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode := r.URL.Query().Get("mode")
		modes = append(modes, mode)
		if mode == "semantic" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"semantic_unavailable","message":"embedder disabled"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode([]RemoteSearchResult{{
			ID:       "m-text",
			Category: "fact",
			Content:  "text result",
		}})
	}))
	defer srv.Close()

	results, err := testSearchClient(srv).SearchRemote(context.Background(), "p1", "needle", 5)
	if err != nil {
		t.Fatalf("SearchRemote: %v", err)
	}
	if len(results) != 1 || results[0].ID != "m-text" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if want := []string{"semantic", "text"}; !reflect.DeepEqual(modes, want) {
		t.Fatalf("attempt modes = %v, want %v", modes, want)
	}
}

func TestSearchRemote_LexicalFallbackFailureIsNotRetried(t *testing.T) {
	var modes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modes = append(modes, r.URL.Query().Get("mode"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":{"code":"semantic_unavailable"}}`))
	}))
	defer srv.Close()

	_, err := testSearchClient(srv).SearchRemote(context.Background(), "p1", "needle", 5)
	if err == nil {
		t.Fatal("expected fallback error")
	}
	if want := []string{"semantic", "text"}; !reflect.DeepEqual(modes, want) {
		t.Fatalf("attempt modes = %v, want exactly %v", modes, want)
	}
}

func TestSearchRemote_DoesNotFallbackForOtherFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "not found", status: http.StatusNotFound, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "request timeout", status: http.StatusRequestTimeout, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "conflict", status: http.StatusConflict, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "too early", status: http.StatusTooEarly, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "server failure", status: http.StatusInternalServerError, body: `{"error":{"code":"semantic_unavailable"}}`},
		{name: "other 422 code", status: http.StatusUnprocessableEntity, body: `{"error":{"code":"invalid_query"}}`},
		{name: "legacy flat error", status: http.StatusUnprocessableEntity, body: `{"error":"semantic_unavailable"}`},
		{name: "malformed 422", status: http.StatusUnprocessableEntity, body: `not-json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := testSearchClient(srv).SearchRemote(context.Background(), "p1", "needle", 5)
			if err == nil {
				t.Fatal("expected error")
			}
			var remoteErr *RemoteError
			if !errors.As(err, &remoteErr) {
				t.Fatalf("error type = %T, want *RemoteError: %v", err, err)
			}
			if _, ok := err.(*RemoteError); !ok {
				t.Fatalf("legacy SearchRemote concrete error type = %T, want *RemoteError", err)
			}
			if remoteErr.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", remoteErr.StatusCode, tt.status)
			}
			if hits != 1 {
				t.Fatalf("request count = %d, want 1", hits)
			}
		})
	}
}

func TestSearchRemote_ContextDeadlineDoesNotRetry(t *testing.T) {
	hit := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	_, err := testSearchClient(srv).SearchRemote(ctx, "p1", "needle", 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}

	if got := len(hit); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestSearchRemote_MalformedSuccessDoesNotFallback(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	_, err := testSearchClient(srv).SearchRemote(context.Background(), "p1", "needle", 5)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if hits != 1 {
		t.Fatalf("request count = %d, want 1", hits)
	}
}

func TestSearchRemote_LegacyServerIgnoringMode(t *testing.T) {
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mode = r.URL.Query().Get("mode")
		_, _ = w.Write([]byte(`[{
			"id":"legacy-id",
			"category":"fact",
			"content":"legacy response",
			"project_id":"p1",
			"updated_at":"2026-07-20T00:00:00Z"
		}]`))
	}))
	defer srv.Close()

	results, err := testSearchClient(srv).SearchRemote(context.Background(), "p1", "needle", 5)
	if err != nil {
		t.Fatalf("SearchRemote: %v", err)
	}
	if mode != "semantic" {
		t.Fatalf("mode = %q, want semantic", mode)
	}
	if len(results) != 1 || results[0].ID != "legacy-id" {
		t.Fatalf("unexpected legacy results: %+v", results)
	}
	resultType := reflect.TypeOf(RemoteSearchResult{})
	if resultType.NumField() != 7 {
		t.Fatalf("RemoteSearchResult field count = %d, want legacy shape with 7 fields", resultType.NumField())
	}
}

func TestSearchRemoteDetailed_DecodesRankingMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{
			"id":"new-id",
			"category":"decision",
			"content":"new response",
			"project_id":"p1",
			"updated_at":"2026-07-20T00:00:00Z",
			"rank":2,
			"score":0.875,
			"effective_mode":"semantic",
			"origin":"remote"
		}]`))
	}))
	defer srv.Close()

	response, err := testSearchClient(srv).SearchRemoteDetailed(context.Background(), "p1", "needle", 5)
	if err != nil {
		t.Fatalf("SearchRemoteDetailed: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("result count = %d, want 1", len(response.Results))
	}
	got := response.Results[0]
	if got.Rank != 2 || got.Score != 0.875 || got.EffectiveMode != "semantic" ||
		got.Origin != "remote" || got.ID != "new-id" {
		t.Fatalf("metadata did not decode: %+v", got)
	}
}

func TestSearchRemote_TransportErrorDoesNotRetry(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	client := testSearchClient(srv)
	srv.Close()

	_, err := client.SearchRemote(context.Background(), "p1", "needle", 5)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if hits != 0 {
		t.Fatalf("server hits = %d, want 0 after close", hits)
	}
}

func testSearchClient(srv *httptest.Server) *Client {
	return &Client{
		httpClient: srv.Client(),
		serverURL:  srv.URL,
		apiKey:     "test-key",
		clientID:   "test-client",
	}
}
