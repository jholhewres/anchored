package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

const federationRRFK = 60

// federationScope maps the local and remote project IDs for the same connected
// repository. The IDs live in different databases, so the mapping is required
// before content fingerprints can safely deduplicate a synced memory.
type federationScope struct {
	LocalProjectID  string
	RemoteProjectID string
}

// federatedSearchHit keeps both source records when local and remote retrieval
// return the same memory. This lets rendering retain provenance without making
// either engine's score comparable to the other engine's score.
type federatedSearchHit struct {
	Local          *memory.SearchResult
	Remote         *remotesync.RemoteSearchHit
	Score          float64
	BestSourceRank int
	LocalRank      int
	RemoteRank     int
	Newest         time.Time
	StableIdentity string
}

func (h federatedSearchHit) origins() []string {
	origins := make([]string, 0, 2)
	if h.Local != nil {
		origins = append(origins, "local")
	}
	if h.Remote != nil {
		origins = append(origins, "remote")
	}
	return origins
}

// federationCandidateLimit bounds work at the federation boundary while
// leaving enough headroom for cross-origin deduplication and rank fusion.
func federationCandidateLimit(resultLimit int) int {
	if resultLimit < 0 {
		resultLimit = 0
	}
	limit := 3 * resultLimit
	if limit < 20 {
		return 20
	}
	return limit
}

// federateSearchResults combines independently ranked local and remote lists
// with reciprocal-rank fusion. It deliberately does not compare raw scores:
// those belong to different engines and are not calibrated against each other.
func federateSearchResults(
	local []memory.SearchResult,
	remote []remotesync.RemoteSearchHit,
	resultLimit int,
	scope federationScope,
) []federatedSearchHit {
	if resultLimit <= 0 {
		return nil
	}

	candidateLimit := federationCandidateLimit(resultLimit)
	if len(local) > candidateLimit {
		local = local[:candidateLimit]
	}
	if len(remote) > candidateLimit {
		remote = remote[:candidateLimit]
	}
	if len(local) == 0 && len(remote) == 0 {
		return nil
	}

	type accumulator struct {
		hit federatedSearchHit
		ids []string
	}

	accumulators := make([]*accumulator, 0, len(local)+len(remote))
	byIdentity := make(map[string]*accumulator, len(local)+len(remote))
	byFingerprint := make(map[string][]*accumulator, len(local)+len(remote))

	add := func(origin string, rank int, localResult *memory.SearchResult, remoteResult *remotesync.RemoteSearchHit) {
		identities, ids := federationIdentities(localResult, remoteResult)
		fingerprint := federationFingerprint(localResult, remoteResult, scope)

		var acc *accumulator
		for _, identity := range identities {
			if existing := byIdentity[identity]; existing != nil {
				acc = existing
				break
			}
		}
		if acc == nil && fingerprint != "" {
			// Content is only a fallback across origins. Equal-content records
			// returned twice by one engine remain distinct unless they share a
			// strong identity.
			for _, existing := range byFingerprint[fingerprint] {
				if (origin == "local" && existing.hit.Local == nil) ||
					(origin == "remote" && existing.hit.Remote == nil) {
					acc = existing
					break
				}
			}
		}
		if acc == nil {
			acc = &accumulator{}
			accumulators = append(accumulators, acc)
			if fingerprint != "" {
				byFingerprint[fingerprint] = append(byFingerprint[fingerprint], acc)
			}
		}

		// A duplicate inside one source contributes at most one rank vote.
		switch origin {
		case "local":
			if acc.hit.Local != nil {
				return
			}
			copied := *localResult
			acc.hit.Local = &copied
			acc.hit.LocalRank = rank
			acc.hit.Newest = newerTime(acc.hit.Newest, localResult.Memory.UpdatedAt, localResult.Memory.CreatedAt)
		case "remote":
			if acc.hit.Remote != nil {
				return
			}
			copied := *remoteResult
			acc.hit.Remote = &copied
			acc.hit.RemoteRank = rank
			acc.hit.Newest = newerTime(acc.hit.Newest, parseRemoteTime(remoteResult.UpdatedAt))
		}

		acc.hit.Score += 1.0 / float64(federationRRFK+rank)
		if acc.hit.BestSourceRank == 0 || rank < acc.hit.BestSourceRank {
			acc.hit.BestSourceRank = rank
		}
		acc.ids = appendUnique(acc.ids, ids...)
		for _, identity := range identities {
			byIdentity[identity] = acc
		}
	}

	for i := range local {
		add("local", i+1, &local[i], nil)
	}
	for i := range remote {
		rank := remote[i].Rank
		if rank <= 0 {
			rank = i + 1
		}
		add("remote", rank, nil, &remote[i])
	}

	hits := make([]federatedSearchHit, 0, len(accumulators))
	for _, acc := range accumulators {
		acc.hit.StableIdentity = stableFederationIdentity(acc.ids, acc.hit)
		hits = append(hits, acc.hit)
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].BestSourceRank != hits[j].BestSourceRank {
			return hits[i].BestSourceRank < hits[j].BestSourceRank
		}
		if !hits[i].Newest.Equal(hits[j].Newest) {
			return hits[i].Newest.After(hits[j].Newest)
		}
		return hits[i].StableIdentity < hits[j].StableIdentity
	})
	if len(hits) > resultLimit {
		hits = hits[:resultLimit]
	}
	return hits
}

func federationIdentities(
	local *memory.SearchResult,
	remote *remotesync.RemoteSearchHit,
) (keys, ids []string) {
	add := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			ids = appendUnique(ids, value)
			keys = appendUnique(keys, "id:"+value)
		}
	}
	if local != nil {
		add(local.Memory.ID)
	}
	if remote != nil {
		add(remote.ID)
	}
	return keys, ids
}

func federationFingerprint(
	local *memory.SearchResult,
	remote *remotesync.RemoteSearchHit,
	scope federationScope,
) string {
	var category, content, projectScope string
	switch {
	case local != nil:
		category = local.Memory.Category
		content = local.Memory.Content
		if local.Memory.ProjectID != nil {
			projectScope = strings.TrimSpace(*local.Memory.ProjectID)
		}
		if projectScope != "" &&
			projectScope == scope.LocalProjectID &&
			scope.RemoteProjectID != "" {
			projectScope = linkedFederationScope(scope)
		}
	case remote != nil:
		category = remote.Category
		content = remote.Content
		projectScope = strings.TrimSpace(remote.ProjectID)
		if projectScope != "" &&
			projectScope == scope.RemoteProjectID &&
			scope.LocalProjectID != "" {
			projectScope = linkedFederationScope(scope)
		}
	}
	category = strings.ToLower(strings.TrimSpace(category))
	content = strings.ToLower(strings.Join(strings.Fields(content), " "))
	if projectScope == "" || category == "" || content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(projectScope + "\x00" + category + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

func linkedFederationScope(scope federationScope) string {
	return "linked:" + scope.LocalProjectID + "\x00" + scope.RemoteProjectID
}

func stableFederationIdentity(ids []string, hit federatedSearchHit) string {
	if len(ids) > 0 {
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		return "id:" + sorted[0]
	}

	// Search records are expected to have IDs. Keep a deterministic final
	// fallback for legacy/malformed responses without inventing a shared ID.
	var category, content string
	if hit.Local != nil {
		category, content = hit.Local.Memory.Category, hit.Local.Memory.Content
	} else if hit.Remote != nil {
		category, content = hit.Remote.Category, hit.Remote.Content
	}
	sum := sha256.Sum256([]byte(category + "\x00" + content + "\x00" + strings.Join(hit.origins(), ",")))
	return "content:" + hex.EncodeToString(sum[:])
}

func parseRemoteTime(raw string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func newerTime(current time.Time, candidates ...time.Time) time.Time {
	for _, candidate := range candidates {
		if candidate.After(current) {
			current = candidate
		}
	}
	return current
}

func appendUnique(values []string, additions ...string) []string {
	for _, addition := range additions {
		if addition == "" {
			continue
		}
		found := false
		for _, value := range values {
			if value == addition {
				found = true
				break
			}
		}
		if !found {
			values = append(values, addition)
		}
	}
	return values
}
