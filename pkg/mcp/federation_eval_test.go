package mcp

import (
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/eval"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

func TestFederationEvaluationScenarios(t *testing.T) {
	const (
		localProject  = "local-project"
		remoteProject = "remote-project"
	)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	local := func(id, content string) memory.SearchResult {
		return localSearchResult(id, localProject, content, now, 1)
	}
	remote := func(id, content string, rank int) remotesync.RemoteSearchHit {
		return remotesync.RemoteSearchHit{
			ID: id, ProjectID: remoteProject, Category: "decision",
			Content: content, UpdatedAt: now.Format(time.RFC3339Nano), Rank: rank,
		}
	}
	scope := federationScope{LocalProjectID: localProject, RemoteProjectID: remoteProject}
	tests := []struct {
		name     string
		local    []memory.SearchResult
		remote   []remotesync.RemoteSearchHit
		expected []string
	}{
		{
			name: "both origins online",
			local: []memory.SearchResult{
				local("local-first", "local result"),
				local("shared", "shared result"),
			},
			remote: []remotesync.RemoteSearchHit{
				remote("shared", "shared result", 1),
				remote("remote-second", "remote result", 2),
			},
			expected: []string{"shared"},
		},
		{
			name:     "semantic unavailable text fallback results",
			local:    []memory.SearchResult{local("z-local", "local result")},
			remote:   []remotesync.RemoteSearchHit{remote("text-fallback", "fallback result", 1)},
			expected: []string{"text-fallback"},
		},
		{
			name:     "remote empty",
			local:    []memory.SearchResult{local("local-only", "local only")},
			expected: []string{"local-only"},
		},
		{
			name:     "local empty",
			remote:   []remotesync.RemoteSearchHit{remote("remote-only", "remote only", 1)},
			expected: []string{"remote-only"},
		},
		{
			name: "cross-origin duplicate",
			local: []memory.SearchResult{
				local("duplicate", "same fact"),
				local("local-second", "another fact"),
			},
			remote: []remotesync.RemoteSearchHit{
				remote("duplicate", "same fact", 1),
				remote("remote-second", "remote fact", 2),
			},
			expected: []string{"duplicate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := federateSearchResults(tt.local, tt.remote, 3, scope)
			ranked := federationHitIDs(first)
			score := eval.ScoreRanking(tt.expected, ranked, 3)
			if score.Recall != 1 || score.MRR != 1 || score.NDCG != 1 {
				t.Fatalf("ranking=%v score=%+v", ranked, score)
			}
			for i := 0; i < 20; i++ {
				repeated := federationHitIDs(federateSearchResults(tt.local, tt.remote, 3, scope))
				if len(repeated) != len(ranked) {
					t.Fatalf("run %d length=%d want %d", i, len(repeated), len(ranked))
				}
				for j := range ranked {
					if repeated[j] != ranked[j] {
						t.Fatalf("run %d ranking=%v want %v", i, repeated, ranked)
					}
				}
			}
		})
	}
}
