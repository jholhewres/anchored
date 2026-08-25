package memory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// SearchOptions.MaxResults must win over the configured default. Reading the
// config unconditionally made the per-call option dead weight: `anchored search
// --limit 100` returned search.max_results rows, and nothing could look deeper
// than the config allowed.
var distinctWords = []string{
	"postgres", "sqlite", "docker", "kubernetes", "redis", "kafka", "nginx",
	"terraform", "ansible", "prometheus", "grafana", "vault", "consul", "etcd",
	"rabbitmq", "elastic", "kibana", "jenkins", "gitlab", "argocd",
}

func TestHybridSearchHonorsPerCallLimit(t *testing.T) {
	store, err := NewSQLiteStore(
		filepath.Join(t.TempDir(), "limit.db"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Every memory shares the query token but is otherwise distinct, so the
	// near-duplicate merge on the save path does not collapse them.
	for i := range 40 {
		if err := store.Save(ctx, Memory{
			ID:       fmt.Sprintf("mem-%02d", i),
			Category: "fact",
			Content: fmt.Sprintf(
				"alpha registro %d sobre %s com detalhe %d e contexto proprio",
				i, distinctWords[i%len(distinctWords)], i*7919),
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	cfg := DefaultHybridSearchConfig()
	// Deliberately tiny: the test asks for more than this and checks the
	// per-call value wins. It cannot assert an exact count — MMR without a
	// vector cache diversifies down on its own — so it asserts the config
	// ceiling no longer binds.
	cfg.MaxResults = 2
	h := NewHybridSearcher(store, nil, nil, nil, cfg, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	t.Run("per-call limit overrides a smaller config default", func(t *testing.T) {
		got, err := h.Search(ctx, "alpha", SearchOptions{MaxResults: 25})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) <= cfg.MaxResults {
			t.Errorf("got %d results, want more than the configured %d — "+
				"the per-call limit was ignored", len(got), cfg.MaxResults)
		}
		if len(got) > 25 {
			t.Errorf("got %d results, want at most the requested 25", len(got))
		}
	})

	t.Run("config default applies when the caller does not ask", func(t *testing.T) {
		got, err := h.Search(ctx, "alpha", SearchOptions{})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) > cfg.MaxResults {
			t.Errorf("got %d results, want at most the configured %d", len(got), cfg.MaxResults)
		}
	})
}
