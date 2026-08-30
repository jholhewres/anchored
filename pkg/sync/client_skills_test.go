package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectSkills_UsesEscapedPathQueryAndAuth(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		if got, want := r.URL.EscapedPath(), "/v1/projects/project%2Fwith%20space/skills"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("q"), "build + deploy & test"; got != want {
			t.Errorf("q = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`[{"id":"s1","name":"Deploy","slug":"deploy","purpose":"Deploy safely","status":"active","version":2,"content_hash":"abc"}]`))
	}))
	defer server.Close()

	skills, err := testSearchClient(server).SearchProjectSkills(context.Background(), "project/with space", "build + deploy & test")
	if err != nil {
		t.Fatalf("SearchProjectSkills: %v", err)
	}
	if calls != 1 || len(skills) != 1 || skills[0].Slug != "deploy" || skills[0].Version != 2 {
		t.Fatalf("calls=%d skills=%+v", calls, skills)
	}
}

func TestLoadProjectSkill_DecodesExactContentAndCapabilityMiss(t *testing.T) {
	const content = "# Deploy\n\nUse the safe path."
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/v1/projects/p%2F1/skills/deploy%2Fsafe"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		_, _ = fmt.Fprintf(w, `{"id":"s1","name":"Deploy","slug":"deploy/safe","purpose":"Deploy safely","status":"active","version":2,"content_hash":"sha256:%X","content":%q}`, sum, content)
	}))
	defer server.Close()

	skill, err := testSearchClient(server).LoadProjectSkill(context.Background(), "p/1", "deploy/safe")
	if err != nil {
		t.Fatalf("LoadProjectSkill: %v", err)
	}
	if skill.Content != content || skill.ContentHash != hash || skill.Version != 2 {
		t.Fatalf("skill = %+v", skill)
	}

	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	_, err = testSearchClient(missing).SearchProjectSkills(context.Background(), "p1", "deploy")
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) || remoteErr.StatusCode != http.StatusNotFound {
		t.Fatalf("404 error = %v, remote=%+v", err, remoteErr)
	}
}

func TestLoadProjectSkill_RejectsMismatchedIdentityAndIntegrity(t *testing.T) {
	const content = "verified content"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	tests := []struct {
		name string
		body string
		want string
	}{
		{"slug", fmt.Sprintf(`{"slug":"other","status":"active","version":1,"content_hash":%q,"content":%q}`, hash, content), "slug does not match"},
		{"inactive", fmt.Sprintf(`{"slug":"deploy","status":"deprecated","version":1,"content_hash":%q,"content":%q}`, hash, content), "not active"},
		{"hash", fmt.Sprintf(`{"slug":"deploy","status":"active","version":1,"content_hash":%q,"content":%q}`, strings.Repeat("0", 64), content), "does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			if _, err := testSearchClient(server).LoadProjectSkill(context.Background(), "p1", "deploy"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestProjectSkills_RejectsOversizedAndMalformedLoadResponses(t *testing.T) {
	overLimit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxProjectSkillsResponseBytes+1)))
	}))
	defer overLimit.Close()
	if _, err := testSearchClient(overLimit).SearchProjectSkills(context.Background(), "p1", "deploy"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized descriptor response error = %v", err)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`null`))
	}))
	defer malformed.Close()
	if _, err := testSearchClient(malformed).LoadProjectSkill(context.Background(), "p1", "deploy"); err == nil || !strings.Contains(err.Error(), "missing slug") {
		t.Fatalf("malformed load response error = %v", err)
	}
}
