package mcp

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

func TestFederationCandidateLimit(t *testing.T) {
	tests := []struct {
		name string
		k    int
		want int
	}{
		{name: "negative", k: -1, want: 20},
		{name: "zero", k: 0, want: 20},
		{name: "small", k: 1, want: 20},
		{name: "boundary", k: 6, want: 20},
		{name: "three times k", k: 7, want: 21},
		{name: "default search", k: 10, want: 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := federationCandidateLimit(tt.k); got != tt.want {
				t.Fatalf("federationCandidateLimit(%d) = %d, want %d", tt.k, got, tt.want)
			}
		})
	}
}

func TestFederateSearchResults_EmptyAndSingleOrigin(t *testing.T) {
	projectID := "local-project"
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	local := []memory.SearchResult{
		localSearchResult("local-1", projectID, "first", now, 0.01),
		localSearchResult("local-2", projectID, "second", now.Add(time.Hour), 100),
	}
	remote := []remotesync.RemoteSearchHit{
		{ID: "remote-1", ProjectID: "remote-project", Content: "first", UpdatedAt: now.Format(time.RFC3339Nano)},
		{ID: "remote-2", ProjectID: "remote-project", Content: "second", UpdatedAt: now.Add(time.Hour).Format(time.RFC3339Nano)},
	}

	if got := federateSearchResults(nil, nil, 10, federationScope{}); got != nil {
		t.Fatalf("empty federation = %#v, want nil", got)
	}
	if got := federateSearchResults(local, nil, 0, federationScope{}); got != nil {
		t.Fatalf("zero limit = %#v, want nil", got)
	}

	localOnly := federateSearchResults(local, nil, 10, federationScope{})
	if got := federationHitIDs(localOnly); !reflect.DeepEqual(got, []string{"local-1", "local-2"}) {
		t.Fatalf("local-only order = %v", got)
	}
	if localOnly[0].Score != 1.0/61.0 || localOnly[1].Score != 1.0/62.0 {
		t.Fatalf("local-only RRF scores = %v, %v", localOnly[0].Score, localOnly[1].Score)
	}
	// Raw engine scores are intentionally not compared at this boundary.
	if localOnly[0].Local.Score != 0.01 || localOnly[1].Local.Score != 100 {
		t.Fatal("local result provenance was not preserved")
	}

	remoteOnly := federateSearchResults(nil, remote, 1, federationScope{})
	if got := federationHitIDs(remoteOnly); !reflect.DeepEqual(got, []string{"remote-1"}) {
		t.Fatalf("remote-only limited order = %v", got)
	}
	if got := remoteOnly[0].origins(); !reflect.DeepEqual(got, []string{"remote"}) {
		t.Fatalf("remote-only origins = %v", got)
	}
}

func TestFederateSearchResults_SharedIdentityDeduplicatesAndAddsRRF(t *testing.T) {
	projectID := "local-project"
	now := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	local := []memory.SearchResult{
		localSearchResult("local-first", projectID, "other", now, 1),
		localSearchResult("shared-global", projectID, "canonical local content", now, 0.5),
	}
	remote := []remotesync.RemoteSearchHit{
		{
			ID:        "shared-global",
			Category:  "decision",
			ProjectID: "remote-project",
			Content:   "canonical remote content",
			UpdatedAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		},
	}

	got := federateSearchResults(local, remote, 10, federationScope{
		LocalProjectID:  projectID,
		RemoteProjectID: "remote-project",
	})
	if len(got) != 2 {
		t.Fatalf("fused count = %d, want 2: %#v", len(got), got)
	}
	shared := got[0]
	if shared.Local == nil || shared.Remote == nil {
		t.Fatalf("shared hit lost provenance: %#v", shared)
	}
	if gotOrigins := shared.origins(); !reflect.DeepEqual(gotOrigins, []string{"local", "remote"}) {
		t.Fatalf("origins = %v", gotOrigins)
	}
	wantScore := 1.0/62.0 + 1.0/61.0
	if math.Abs(shared.Score-wantScore) > 1e-15 {
		t.Fatalf("RRF score = %.17f, want %.17f", shared.Score, wantScore)
	}
	if shared.BestSourceRank != 1 || shared.LocalRank != 2 || shared.RemoteRank != 1 {
		t.Fatalf("ranks = best:%d local:%d remote:%d", shared.BestSourceRank, shared.LocalRank, shared.RemoteRank)
	}
	if shared.StableIdentity != "id:shared-global" {
		t.Fatalf("stable identity = %q", shared.StableIdentity)
	}
	if !shared.Newest.Equal(now.Add(time.Hour)) {
		t.Fatalf("newest = %s", shared.Newest)
	}
}

func TestFederateSearchResults_ProjectScopedFingerprint(t *testing.T) {
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	localProjectID := "local-project"
	local := []memory.SearchResult{
		localSearchResult("local-id", localProjectID, "  Keep   Semantic DEFAULT  ", now, 1),
	}
	remote := []remotesync.RemoteSearchHit{
		{
			ID:        "different-remote-id",
			Category:  "decision",
			ProjectID: "remote-project",
			Content:   "keep semantic default",
			UpdatedAt: now.Format(time.RFC3339Nano),
		},
	}

	linked := federateSearchResults(local, remote, 10, federationScope{
		LocalProjectID:  localProjectID,
		RemoteProjectID: "remote-project",
	})
	if len(linked) != 1 || linked[0].Local == nil || linked[0].Remote == nil {
		t.Fatalf("linked fingerprint did not merge: %#v", linked)
	}

	unrelated := federateSearchResults(local, remote, 10, federationScope{
		LocalProjectID:  "another-local-project",
		RemoteProjectID: "another-remote-project",
	})
	if len(unrelated) != 2 {
		t.Fatalf("unrelated project fingerprints merged: %#v", unrelated)
	}

	differentCategory := append([]remotesync.RemoteSearchHit(nil), remote...)
	differentCategory[0].Category = "learning"
	notSameFact := federateSearchResults(local, differentCategory, 10, federationScope{
		LocalProjectID:  localProjectID,
		RemoteProjectID: "remote-project",
	})
	if len(notSameFact) != 2 {
		t.Fatalf("equal content from different categories merged: %#v", notSameFact)
	}
}

func TestFederateSearchResults_DeterministicTieBreak(t *testing.T) {
	projectID := "local-project"
	base := time.Date(2026, 7, 20, 4, 0, 0, 0, time.UTC)

	// All four hits have the same RRF score and best source rank. Newest time
	// decides first; stable identity decides the remaining exact ties.
	local := []memory.SearchResult{
		localSearchResult("id-b", projectID, "b", base, 1),
	}
	remote := []remotesync.RemoteSearchHit{
		{
			ID:        "id-a",
			ProjectID: "remote-project",
			Content:   "a",
			UpdatedAt: base.Format(time.RFC3339Nano),
		},
	}
	first := federateSearchResults(local, remote, 10, federationScope{})
	if got := federationHitIDs(first); !reflect.DeepEqual(got, []string{"id-a", "id-b"}) {
		t.Fatalf("stable identity tie-break = %v", got)
	}

	remote[0].UpdatedAt = base.Add(time.Minute).Format(time.RFC3339Nano)
	second := federateSearchResults(local, remote, 10, federationScope{})
	if got := federationHitIDs(second); !reflect.DeepEqual(got, []string{"id-a", "id-b"}) {
		t.Fatalf("newest tie-break = %v", got)
	}

	for i := 0; i < 20; i++ {
		repeated := federateSearchResults(local, remote, 10, federationScope{})
		if !reflect.DeepEqual(federationHitIDs(repeated), federationHitIDs(second)) {
			t.Fatalf("run %d order changed: %v", i, federationHitIDs(repeated))
		}
	}
}

func TestFederateSearchResults_DuplicateWithinOriginVotesOnce(t *testing.T) {
	projectID := "local-project"
	local := []memory.SearchResult{
		localSearchResult("same-id", projectID, "first representation", time.Time{}, 1),
		localSearchResult("same-id", projectID, "duplicate representation", time.Time{}, 2),
	}
	got := federateSearchResults(local, nil, 10, federationScope{})
	if len(got) != 1 {
		t.Fatalf("duplicate source count = %d, want 1", len(got))
	}
	if got[0].Score != 1.0/61.0 || got[0].LocalRank != 1 {
		t.Fatalf("duplicate source added a second vote: %#v", got[0])
	}
	if got[0].Local.Memory.Content != "first representation" {
		t.Fatalf("duplicate source replaced the best-ranked representation: %#v", got[0].Local)
	}
}

func TestFederateSearchResults_RemoteDeclaredAndLegacyRanks(t *testing.T) {
	remote := []remotesync.RemoteSearchHit{
		{
			ID:        "declared-rank",
			Category:  "decision",
			ProjectID: "remote-project",
			Content:   "server supplied rank",
			UpdatedAt: "not-a-timestamp",
			Rank:      7,
		},
		{
			ID:        "legacy-rank",
			Category:  "decision",
			ProjectID: "remote-project",
			Content:   "legacy position rank",
			UpdatedAt: "",
		},
	}
	got := federateSearchResults(nil, remote, 10, federationScope{})
	if len(got) != 2 {
		t.Fatalf("remote rank result count = %d", len(got))
	}

	// The legacy rank=0 item falls back to its original 1-based position (2)
	// and therefore outranks the server-declared rank 7 item.
	if ids := federationHitIDs(got); !reflect.DeepEqual(ids, []string{"legacy-rank", "declared-rank"}) {
		t.Fatalf("remote rank order = %v", ids)
	}
	if got[0].RemoteRank != 2 || got[0].Score != 1.0/62.0 {
		t.Fatalf("legacy rank fallback = rank %d score %.17f", got[0].RemoteRank, got[0].Score)
	}
	if got[1].RemoteRank != 7 || got[1].Score != 1.0/67.0 {
		t.Fatalf("declared rank = rank %d score %.17f", got[1].RemoteRank, got[1].Score)
	}
	if !got[0].Newest.IsZero() || !got[1].Newest.IsZero() {
		t.Fatalf("malformed timestamps should be zero: %s %s", got[0].Newest, got[1].Newest)
	}
}

func TestFederateSearchResults_BoundsCandidatesBeforeFusion(t *testing.T) {
	projectID := "local-project"
	local := make([]memory.SearchResult, 0, 22)
	for i := 1; i <= 22; i++ {
		local = append(local, localSearchResult(
			"local-"+string(rune('a'+i-1)),
			projectID,
			"local candidate",
			time.Time{},
			1,
		))
	}
	// K=7 permits exactly 21 candidates per origin. The 22nd local candidate
	// shares an ID with the remote hit; it must not contribute a local vote.
	local[21].Memory.ID = "shared-beyond-cap"
	remote := []remotesync.RemoteSearchHit{
		{
			ID:        "shared-beyond-cap",
			Category:  "decision",
			ProjectID: "remote-project",
			Content:   "remote candidate",
			Rank:      1,
		},
	}
	got := federateSearchResults(local, remote, 7, federationScope{})
	var shared *federatedSearchHit
	for i := range got {
		if got[i].Remote != nil && got[i].Remote.ID == "shared-beyond-cap" {
			shared = &got[i]
			break
		}
	}
	if shared == nil {
		t.Fatalf("remote rank-1 candidate missing from bounded result: %#v", got)
	}
	if shared.Local != nil || shared.Score != 1.0/61.0 {
		t.Fatalf("candidate beyond cap contributed to fusion: %#v", *shared)
	}
}

func localSearchResult(id, projectID, content string, updatedAt time.Time, score float64) memory.SearchResult {
	return memory.SearchResult{
		Memory: memory.Memory{
			ID:        id,
			ProjectID: &projectID,
			Category:  "decision",
			Content:   content,
			CreatedAt: updatedAt.Add(-time.Hour),
			UpdatedAt: updatedAt,
		},
		Score: score,
	}
}

func federationHitIDs(hits []federatedSearchHit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		switch {
		case hit.Local != nil:
			ids = append(ids, hit.Local.Memory.ID)
		case hit.Remote != nil:
			ids = append(ids, hit.Remote.ID)
		}
	}
	return ids
}
