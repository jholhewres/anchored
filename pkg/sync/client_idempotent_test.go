package sync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSaveRemoteIdempotent_SendsOperationIDAndReturnsLegacyResponse(t *testing.T) {
	var operationIDs []string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		operationIDs = append(operationIDs, r.Header.Get("Idempotency-Key"))
		if requests == 2 {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"remote-1","category":"fact","project_id":"project-1","created":true}`))
	}))
	defer srv.Close()

	client := testRemoteSaveClient(srv)
	mem := testRemoteMemory()
	first, err := client.SaveRemoteIdempotent(context.Background(), mem, "operation-123")
	if err != nil {
		t.Fatalf("first SaveRemoteIdempotent: %v", err)
	}
	replay, err := client.SaveRemoteIdempotent(context.Background(), mem, "operation-123")
	if err != nil {
		t.Fatalf("replayed SaveRemoteIdempotent: %v", err)
	}

	if want := []string{"operation-123", "operation-123"}; !reflect.DeepEqual(operationIDs, want) {
		t.Fatalf("operation IDs = %v, want %v", operationIDs, want)
	}
	if replay.ID != first.ID || replay.ProjectID != first.ProjectID {
		t.Fatalf("replay response = %+v, want same identity as %+v", replay, first)
	}
	if got := reflect.TypeOf(SaveRemoteResponse{}).NumField(); got != 4 {
		t.Fatalf("SaveRemoteResponse field count = %d, want legacy shape with 4 fields", got)
	}
}

func TestSaveRemoteIdempotent_EmptyOperationIDOmitHeader(t *testing.T) {
	var headerPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, headerPresent = r.Header["Idempotency-Key"]
		_, _ = w.Write([]byte(`{"id":"remote-1","category":"fact","created":true}`))
	}))
	defer srv.Close()

	if _, err := testRemoteSaveClient(srv).SaveRemoteIdempotent(
		context.Background(),
		testRemoteMemory(),
		"",
	); err != nil {
		t.Fatalf("SaveRemoteIdempotent: %v", err)
	}
	if headerPresent {
		t.Error("empty operation ID sent an Idempotency-Key header")
	}
}

func TestSaveRemote_RemainsNonIdempotent(t *testing.T) {
	t.Run("success ignores idempotency metadata", func(t *testing.T) {
		var headerPresent bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, headerPresent = r.Header["Idempotency-Key"]
			w.Header().Set("Idempotency-Replayed", "true")
			_, _ = w.Write([]byte(`{"id":"remote-1","category":"fact","created":true}`))
		}))
		defer srv.Close()

		response, err := testRemoteSaveClient(srv).SaveRemote(context.Background(), testRemoteMemory())
		if err != nil {
			t.Fatalf("SaveRemote: %v", err)
		}
		if headerPresent {
			t.Error("legacy SaveRemote sent an Idempotency-Key header")
		}
		if response.ID != "remote-1" {
			t.Fatalf("legacy SaveRemote response = %+v", response)
		}
	})

	t.Run("error retains legacy field values", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"internal_error"}}`))
		}))
		defer srv.Close()

		_, err := testRemoteSaveClient(srv).SaveRemote(context.Background(), testRemoteMemory())
		var remoteErr *RemoteError
		if !errors.As(err, &remoteErr) {
			t.Fatalf("error = %v, want *RemoteError", err)
		}
		code, retryAfter := RemoteErrorMetadata(err)
		if code != "" || retryAfter != "" {
			t.Fatalf("legacy SaveRemote unexpectedly populated metadata: code=%q retry_after=%q", code, retryAfter)
		}
	})
}

func TestSaveRemoteIdempotent_ConflictMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"idempotency_conflict","message":"operation payload changed"}}`))
	}))
	defer srv.Close()

	_, err := testRemoteSaveClient(srv).SaveRemoteIdempotent(
		context.Background(),
		testRemoteMemory(),
		"operation-conflict",
	)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("error type = %T, want *RemoteError: %v", err, err)
	}
	code, _ := RemoteErrorMetadata(err)
	if remoteErr.StatusCode != http.StatusConflict || code != "idempotency_conflict" {
		t.Fatalf("remote error = %+v", remoteErr)
	}
	if remoteErr.Error() != `remote server returned 409: {"error":{"code":"idempotency_conflict","message":"operation payload changed"}}` {
		t.Fatalf("legacy error text changed: %q", remoteErr.Error())
	}
}

func TestSaveRemoteIdempotent_RetryAfterMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"rate_limited","error":"retry later"}`))
	}))
	defer srv.Close()

	_, err := testRemoteSaveClient(srv).SaveRemoteIdempotent(
		context.Background(),
		testRemoteMemory(),
		"operation-rate-limited",
	)
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("error = %v, want *RemoteError", err)
	}
	code, retryAfter := RemoteErrorMetadata(err)
	if remoteErr.StatusCode != http.StatusTooManyRequests ||
		code != "rate_limited" ||
		retryAfter != "17" {
		t.Fatalf("remote error metadata = %+v", remoteErr)
	}
}

func TestSaveRemoteIdempotent_RejectsUnsafeErrorMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "Bearer secret-value")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"token secret-value"}}`))
	}))
	defer srv.Close()

	_, err := testRemoteSaveClient(srv).SaveRemoteIdempotent(
		context.Background(),
		testRemoteMemory(),
		"operation-unsafe-error",
	)
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("error = %v, want *RemoteError", err)
	}
	code, retryAfter := RemoteErrorMetadata(err)
	if code != "" || retryAfter != "" {
		t.Fatalf("unsafe metadata was exposed: code=%q retry_after=%q",
			code, retryAfter)
	}
}

func TestSaveRemoteIdempotent_HTTPErrorMatrix(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{name: "bad request", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_memory"}}`, code: "invalid_memory"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"code":"forbidden"}`, code: "forbidden"},
		{name: "request timeout", status: http.StatusRequestTimeout, body: `{"error":{"code":"request_timeout"}}`, code: "request_timeout"},
		{name: "too early", status: http.StatusTooEarly, body: `{"error":{"code":"too_early"}}`, code: "too_early"},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":{"code":"internal_error"}}`, code: "internal_error"},
		{name: "unavailable", status: http.StatusServiceUnavailable, body: `not-json`, code: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := testRemoteSaveClient(srv).SaveRemoteIdempotent(
				context.Background(),
				testRemoteMemory(),
				"operation-error",
			)
			var remoteErr *RemoteError
			if !errors.As(err, &remoteErr) {
				t.Fatalf("error = %v, want *RemoteError", err)
			}
			code, _ := RemoteErrorMetadata(err)
			if remoteErr.StatusCode != tt.status || code != tt.code ||
				remoteErr.Body != tt.body {
				t.Fatalf("remote error = %+v, want status=%d code=%q body=%q",
					remoteErr, tt.status, tt.code, tt.body)
			}
		})
	}
}

func TestSaveRemoteIdempotent_TransportError(t *testing.T) {
	transportErr := errors.New("connection reset")
	client := &Client{
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})},
		serverURL: "http://remote.example",
	}

	_, err := client.SaveRemoteIdempotent(
		context.Background(),
		testRemoteMemory(),
		"operation-transport",
	)
	if !errors.Is(err, transportErr) {
		t.Fatalf("error = %v, want wrapped transport error", err)
	}
	var remoteErr *RemoteError
	if errors.As(err, &remoteErr) {
		t.Fatalf("transport error unexpectedly classified as RemoteError: %+v", remoteErr)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testRemoteSaveClient(srv *httptest.Server) *Client {
	return &Client{
		httpClient: srv.Client(),
		serverURL:  srv.URL,
		apiKey:     "test-key",
		clientID:   "test-client",
	}
}

func testRemoteMemory() RemoteMemory {
	return RemoteMemory{
		ID:        "memory-1",
		Category:  "fact",
		Content:   "Postgres persists the remote memory",
		ProjectID: "project-1",
	}
}
