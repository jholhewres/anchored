package ctx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetcher_HTMLToMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h1>Hello</h1><p>World</p></body></html>")
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)
	result, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FromCache {
		t.Error("should not be from cache")
	}
	if !strings.Contains(result.Markdown, "Hello") {
		t.Errorf("expected markdown to contain 'Hello', got: %s", result.Markdown)
	}
	if !strings.Contains(result.Markdown, "World") {
		t.Errorf("expected markdown to contain 'World', got: %s", result.Markdown)
	}
	if !strings.Contains(result.ContentType, "text/html") {
		t.Errorf("expected content type text/html, got: %s", result.ContentType)
	}
}

func TestFetcher_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "plain text content")
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)
	result, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Markdown != "plain text content" {
		t.Errorf("expected 'plain text content', got: %s", result.Markdown)
	}
}

func TestFetcher_JSONResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"key":"value","nested":{"a":1}}`)
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)
	result, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Markdown, `"key": "value"`) {
		t.Errorf("expected pretty-printed JSON, got: %s", result.Markdown)
	}
}

func TestFetcher_CacheHit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "call %d", calls)
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)

	result1, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if result1.FromCache {
		t.Error("first fetch should not be from cache")
	}

	result2, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if !result2.FromCache {
		t.Error("second fetch should be from cache")
	}
	if result2.Markdown != result1.Markdown {
		t.Errorf("cache mismatch: %q vs %q", result1.Markdown, result2.Markdown)
	}
	if calls != 1 {
		t.Errorf("expected 1 server call, got %d", calls)
	}
}

func TestFetcher_404Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)
	_, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404: %v", err)
	}
}

func TestFetcher_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := NewFetcher(50*time.Millisecond, 5*time.Minute, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := f.FetchAndConvert(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFetcher_LargeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		// 11MB of data
		w.Write(make([]byte, 11*1024*1024))
	}))
	defer srv.Close()

	f := NewFetcher(10*time.Second, 5*time.Minute, nil)
	_, err := f.FetchAndConvert(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected error for large response")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size limit: %v", err)
	}
}
