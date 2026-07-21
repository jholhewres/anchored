package mcp

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

func TestIdentityHasContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "factory template",
			content: "# Identity\n\n## About Me\n- Name: \n- Role: \n- Preferences: \n\n## Projects\n-",
			want:    false,
		},
		{name: "headers only", content: "# Identity\n## About Me\n## Projects", want: false},
		{name: "empty", content: "", want: false},
		{name: "lone markers", content: "-\n*\n+", want: false},
		{name: "filled name", content: "# Identity\n## About Me\n- Name: Jhol\n- Role: ", want: true},
		{name: "free prose", content: "I prefer Go and small diffs.", want: true},
		{name: "bullet with value", content: "- Preferences: PT-BR, concise", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityHasContent(tc.content); got != tc.want {
				t.Fatalf("IdentityHasContent(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// newIdentityTestServer builds a Server backed by an on-disk sqlite service
// with embeddings disabled, so synthesizeIdentity can be exercised end-to-end.
func newIdentityTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "test.db")
	cfg.Embedding.Provider = "none"

	svc, err := memory.NewService(cfg, slog.Default())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Close)
	return NewServer(svc, nil, nil, nil, cfg, "test", slog.Default())
}

func TestSynthesizeIdentity_FromPreferences(t *testing.T) {
	s := newIdentityTestServer(t)
	ctx := context.Background()

	// No preferences yet → empty synthesis.
	if got := s.synthesizeIdentity(ctx, nil); got != "" {
		t.Fatalf("expected empty synthesis with no preferences, got %q", got)
	}

	for _, c := range []string{
		"Respond in PT-BR by default, concise and technical",
		"Never commit without explicit request",
		"Prefer small thematic commits",
	} {
		if _, err := s.mem.Save(ctx, c, "preference", "user", ""); err != nil {
			t.Fatalf("save preference: %v", err)
		}
	}

	out := s.synthesizeIdentity(ctx, nil)
	if !strings.Contains(out, "## Preferences (learned)") {
		t.Fatalf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "anchored identity edit") {
		t.Fatalf("missing curated-identity footer:\n%s", out)
	}
	if !strings.Contains(out, "PT-BR") {
		t.Fatalf("expected a saved preference in the block:\n%s", out)
	}
}

func TestSynthesizeIdentity_HonorsExcludeAndCap(t *testing.T) {
	s := newIdentityTestServer(t)
	ctx := context.Background()

	// Save more than the 5-item cap to prove truncation.
	ids := make([]string, 0, 7)
	for i, c := range []string{
		"pref alpha unique-token-a", "pref bravo unique-token-b", "pref charlie unique-token-c",
		"pref delta unique-token-d", "pref echo unique-token-e", "pref foxtrot unique-token-f",
		"pref golf unique-token-g",
	} {
		m, err := s.mem.Save(ctx, c, "preference", "user", "")
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		ids = append(ids, m.ID)
	}

	// At most 5 bullets.
	out := s.synthesizeIdentity(ctx, nil)
	if n := strings.Count(out, "\n- "); n > 5 {
		t.Fatalf("expected <=5 bullets, got %d:\n%s", n, out)
	}

	// Excluding an id must drop it from the block.
	exclude := map[string]bool{ids[0]: true, ids[1]: true, ids[2]: true, ids[3]: true, ids[4]: true, ids[5]: true}
	got := s.synthesizeIdentity(ctx, exclude)
	for _, id := range []string{ids[0], ids[1], ids[2], ids[3], ids[4], ids[5]} {
		m, _ := s.mem.Get(ctx, id)
		if m != nil && strings.Contains(got, m.Content) {
			t.Fatalf("excluded content leaked into synthesis: %q\n%s", m.Content, got)
		}
	}
}
