package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchRemoteDetailedPreservesEmptyResultMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("mode") != "semantic" {
			t.Fatalf("mode=%q", r.URL.Query().Get("mode"))
		}
		w.Header().Set("X-Anchored-Effective-Mode", "semantic")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	client := testRemoteSaveClient(server)
	response, err := client.SearchRemoteDetailed(context.Background(), "project", "query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 || response.RequestedMode != "semantic" ||
		response.EffectiveMode != "semantic" || response.Fallback {
		t.Fatalf("response=%+v", response)
	}
}

func TestSearchRemoteDetailedDeclaresCapabilityFallback(t *testing.T) {
	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"semantic_unavailable"}}`))
			return
		}
		w.Header().Set("X-Anchored-Effective-Mode", "text")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	response, err := testRemoteSaveClient(server).SearchRemoteDetailed(
		context.Background(), "project", "query", 5,
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempt != 2 || response.RequestedMode != "semantic" ||
		response.EffectiveMode != "text" || !response.Fallback ||
		response.FallbackReason != "semantic_unavailable" {
		t.Fatalf("attempt=%d response=%+v", attempt, response)
	}
}

func TestSearchRemoteDetailedPreservesRuntimeFailureTelemetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Anchored-Effective-Mode", "semantic")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"semantic_query_failed"}}`))
	}))
	defer server.Close()
	response, err := testRemoteSaveClient(server).SearchRemoteDetailed(
		context.Background(), "project", "query", 5,
	)
	var remoteErr *RemoteError
	code, _ := RemoteErrorMetadata(err)
	if !errors.As(err, &remoteErr) || code != "semantic_query_failed" {
		t.Fatalf("error=%v remote=%+v", err, remoteErr)
	}
	if response == nil || response.RequestedMode != "semantic" ||
		response.EffectiveMode != "semantic" || response.Fallback {
		t.Fatalf("response=%+v", response)
	}
}

func TestSearchRemoteDetailedPreservesFallbackReasonWhenTextFails(t *testing.T) {
	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error":{"code":"semantic_unavailable"}}`))
			return
		}
		w.Header().Set("X-Anchored-Effective-Mode", "text")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"text_search_failed"}}`))
	}))
	defer server.Close()
	response, err := testRemoteSaveClient(server).SearchRemoteDetailed(
		context.Background(), "project", "query", 5,
	)
	var remoteErr *RemoteError
	code, _ := RemoteErrorMetadata(err)
	if !errors.As(err, &remoteErr) || code != "text_search_failed" {
		t.Fatalf("error=%v remote=%+v", err, remoteErr)
	}
	if response == nil || response.RequestedMode != "semantic" ||
		response.EffectiveMode != "text" || !response.Fallback ||
		response.FallbackReason != "semantic_unavailable" {
		t.Fatalf("response=%+v", response)
	}
}
