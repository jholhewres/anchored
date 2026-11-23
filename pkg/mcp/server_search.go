package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

func (s *Server) toolSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Query      string `json:"query"`
		CWD        string `json:"cwd"`
		Category   string `json:"category"`
		MaxResults int    `json:"max_results"`
		// Remote distinguishes absent (nil → local + automatic remote
		// merge) from explicitly set (remote-only; "" / "_" / "default"
		// → default remote, anything else → named remote). The old
		// string field made `remote: ""` silently mean "local", which
		// contradicted the tool schema.
		Remote *string `json:"remote"`
		// SessionID, when provided, lets the search boost memories that overlap
		// the session's working set (files/symbols/entities in focus).
		SessionID string `json:"session_id"`
		// Debug surfaces per-result ranking signals and scores.
		Debug bool `json:"debug"`
		// Full lifts the per-hit preview cap (700 runes → 4096) for reading
		// one specific memory in full. Pair with a low max_results.
		Full bool `json:"full"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	limit := p.MaxResults
	if limit <= 0 {
		limit = 10
	}
	searchCandidateLimit := limit
	if p.Remote == nil && s.cfg != nil {
		searchCandidateLimit = federationCandidateLimit(limit)
	}

	// Remote routing needs a real directory. An omitted cwd means "global
	// LOCAL search", but it must not also silently mean "no team memory" —
	// fall back to the session's root, the same convention toolSave uses.
	p.CWD = s.rootedPath(ctx, p.CWD)
	remoteCWD := s.rootedCWD(ctx, p.CWD)

	// Explicit remote: query ONLY the remote server. A failure here must be
	// VISIBLE in the output — falling back to local silently makes the agent
	// present local results as "the remote" and conclude things like "they
	// are in sync" when the remote was never reached. remoteFallback* mark
	// the local output when that happens.
	var remoteFallbackName, remoteFallbackErr string
	if p.Remote != nil && s.cfg != nil {
		selector := *p.Remote
		var entry *config.RemoteEntry
		projectID := ""
		autoRouted := false
		// The repo's remote is chosen from the directory the probe uses,
		// so a glob that routes it also bounds the fallback below.
		routeCWD := p.CWD
		if selector == "" || selector == "_" || selector == "default" {
			routeCWD = remoteCWD
			// "the repo's remote": resolve exactly like sync routing does
			// (git-origin probe across every configured remote), so a repo
			// that syncs to a named server is searched THERE — not on the
			// default config entry, which may have never heard of it.
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			entry, projectID = s.resolveAutoRemoteTarget(rctx, remoteCWD)
			rcancel()
			autoRouted = entry != nil
			if entry == nil {
				entry = s.resolveRemoteEntry(selector, routeCWD)
			}
		} else {
			entry = s.resolveRemoteEntry(selector, p.CWD)
		}

		switch entry {
		case nil:
			remoteFallbackName = selector
			remoteFallbackErr = "remote not configured"
		default:
			client := remotesync.NewClientFromEntry(*entry, "mcp")
			if projectID == "" {
				// Resolve the REMOTE project — the server only knows its own
				// IDs. Same precedence as sync: git-origin remote_key first,
				// linked project for non-repo contexts.
				projectID, _ = s.resolveRemoteProjectID(ctx, client, *entry, routeCWD)
			}
			if projectID == "" {
				remoteFallbackName = entry.Name
				remoteFallbackErr = "project not resolved on this remote (not linked or never synced)"
				break
			}
			response, err := client.SearchRemoteDetailed(ctx, projectID, p.Query, limit)
			if err != nil {
				if autoRouted && routingMayBeStale(err) {
					s.forgetAutoRemoteRead(remoteCWD)
				}
				remoteFallbackName = entry.Name
				switch {
				case remotesync.IsRemoteForbidden(err):
					remoteFallbackErr = "forbidden (check API key and scope)"
				case remotesync.IsRemoteUnavailable(err):
					remoteFallbackErr = "unreachable"
				default:
					remoteFallbackErr = "search failed"
				}
				s.logger.Warn("remote search failed, returning local results marked as fallback",
					"remote", entry.Name, "error", err)
				break
			}
			results := response.Results
			modeAttrs := remoteSearchMetadataAttrs(response)
			if len(results) == 0 {
				return "<anchored_search count=\"0\" remote=\"" +
					escapeAttr(entry.Name) + "\"" + modeAttrs + "/>", nil
			}
			w := newSearchHitWriter(p.Full)
			w.open("<anchored_search query=%q count=%q remote=%q%s>\n",
				truncateRunes(p.Query, 200), fmt.Sprintf("%d", len(results)),
				escapeAttr(entry.Name), modeAttrs)
			for _, r := range results {
				w.hit([]string{
					fmt.Sprintf("id=%q", escapeAttr(r.ID)),
					fmt.Sprintf("category=%q", escapeAttr(r.Category)),
					fmt.Sprintf("project=%q", escapeAttr(r.ProjectID)),
				}, r.Content)
			}
			return w.close(), nil
		}
	}

	// Local search (default path, also fallback from explicit-remote failure)
	var projectID, boostProjectID string
	if p.CWD != "" {
		projectID = s.mem.ResolveProject(p.CWD)
		boostProjectID = projectID
	}

	searchOpts := memory.SearchOptions{
		// This is the requested federation depth. The configured local hybrid
		// engine may enforce a smaller internal top-k; federation never assumes
		// that more local candidates were returned and bounds its inputs again.
		MaxResults:     searchCandidateLimit,
		Category:       p.Category,
		ProjectID:      projectID,
		BoostProjectID: boostProjectID,
		ExplainSignals: p.Debug,
	}
	if p.SessionID != "" && s.sessions != nil {
		if ws, wErr := s.sessions.GetWorkingSet(ctx, p.SessionID); wErr == nil && !ws.Empty() {
			searchOpts.WorkingSet = &memory.WorkingSetSignals{
				Files:    ws.Files,
				Symbols:  ws.Symbols,
				Entities: ws.Entities,
			}
		}
	}

	results, err := s.mem.Search(ctx, p.Query, searchOpts)
	if err != nil {
		return "", err
	}

	// Day-to-day flow: when the cwd's remote is configured, the team memory
	// is part of every search — merge remote hits in automatically (deduped
	// and fused by source rank). Best-effort with a short timeout so an
	// unreachable remote never stalls the tool.
	var remoteHits []remotesync.RemoteSearchHit
	var remoteName, remoteProjectID string
	var remoteSearchMeta *remotesync.RemoteSearchResponse
	if p.Remote == nil && s.cfg != nil {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if target, rpid := s.resolveAutoRemoteTarget(rctx, remoteCWD); target == nil || rpid == "" {
			// Leave a trace: a configured-but-unresolved remote is the #1
			// "why is team memory missing from my search?" report.
			if s.hasAnyRemote() {
				s.logger.Debug("auto remote merge: no project resolved for cwd", "cwd", remoteCWD)
			}
		} else {
			client := remotesync.NewClientFromEntry(*target, "mcp")
			if response, rErr := client.SearchRemoteDetailed(rctx, rpid, p.Query, searchCandidateLimit); rErr == nil {
				for i, r := range response.Results {
					// Preserve the server's declared rank. Legacy servers omit
					// it, so capture the original list position before local
					// category filtering; filtered-out hits must not promote
					// the remaining remote candidates.
					if r.Rank <= 0 {
						r.Rank = i + 1
					}
					if p.Category != "" && r.Category != p.Category {
						continue
					}
					remoteHits = append(remoteHits, r)
				}
				remoteName = target.Name
				remoteProjectID = rpid
				remoteSearchMeta = response
			} else {
				s.logger.Warn("remote merge skipped", "remote", target.Name, "error", rErr)
				if routingMayBeStale(rErr) {
					// The cached remote may have gone down: the next search
					// resolves again and can fall back. A write hold stands.
					s.forgetAutoRemoteRead(remoteCWD)
				}
			}
		}
		cancel()
	}

	// Explicit-remote failure: every local-rendered shape carries the
	// remote_error + fallback attributes so the agent reports "the remote
	// search failed (reason); showing local results" instead of presenting
	// local data as remote.
	fallbackAttrs := ""
	if remoteFallbackErr != "" {
		fallbackAttrs = fmt.Sprintf(" remote=%q remote_error=%q fallback=\"local\"",
			escapeAttr(remoteFallbackName), escapeAttr(remoteFallbackErr))
	} else if remoteName != "" {
		fallbackAttrs = fmt.Sprintf(" remote=%q%s",
			escapeAttr(remoteName), remoteSearchMetadataAttrs(remoteSearchMeta))
	}

	if len(results) == 0 && len(remoteHits) == 0 {
		return "<anchored_search count=\"0\"" + fallbackAttrs + "/>", nil
	}

	if len(remoteHits) == 0 {
		if len(results) > limit {
			results = results[:limit]
		}
		out := renderSearchResults(p.Query, p.CWD == "", results, p.Debug, p.Full)
		if fallbackAttrs != "" {
			out = strings.Replace(out, "<anchored_search ", "<anchored_search"+fallbackAttrs+" ", 1)
		}
		return out, nil
	}

	localFederationProjectID := projectID
	if localFederationProjectID == "" {
		// An omitted cwd keeps local retrieval global, but the automatic remote
		// still belongs to the MCP server's repository. Map only that local
		// project to the remote project for conservative fingerprint dedup.
		localFederationProjectID = s.mem.ResolveProject(remoteCWD)
	}
	fused := federateSearchResults(results, remoteHits, limit, federationScope{
		LocalProjectID:  localFederationProjectID,
		RemoteProjectID: remoteProjectID,
	})

	// Merged rendering preserves the existing hit shape. Remote-only hits keep
	// origin="remote"; a deduplicated cross-origin hit declares both sources.
	globalMode := p.CWD == ""
	w := newSearchHitWriter(p.Full)
	w.open("<anchored_search query=%q count=%q remote=%q%s>\n",
		truncateRunes(p.Query, 200), fmt.Sprintf("%d", len(fused)),
		escapeAttr(remoteName), remoteSearchMetadataAttrs(remoteSearchMeta))
	for _, hit := range fused {
		if hit.Local != nil {
			r := hit.Local
			attrs := []string{
				fmt.Sprintf("id=%q", escapeAttr(r.Memory.ID)),
				fmt.Sprintf("category=%q", escapeAttr(r.Memory.Category)),
				fmt.Sprintf("score=%q", fmt.Sprintf("%.6f", hit.Score)),
			}
			if hit.Remote != nil {
				attrs = append(attrs, fmt.Sprintf("origin=%q", strings.Join(hit.origins(), ",")))
			}
			if globalMode && r.Memory.ProjectID != nil && *r.Memory.ProjectID != "" {
				attrs = append(attrs, fmt.Sprintf("project=%q", escapeAttr(*r.Memory.ProjectID)))
			}
			if p.Debug && len(r.Signals) > 0 {
				attrs = append(attrs, fmt.Sprintf("signals=%q", escapeAttr(strings.Join(r.Signals, ","))))
			}
			if p.Debug {
				attrs = append(attrs,
					fmt.Sprintf("origins=%q", strings.Join(hit.origins(), ",")),
					fmt.Sprintf("rrf_ranks=%q", federatedRankSummary(hit)),
				)
			}
			w.hit(attrs, r.Memory.Content)
			continue
		}
		r := hit.Remote
		attrs := []string{
			fmt.Sprintf("id=%q", escapeAttr(r.ID)),
			fmt.Sprintf("category=%q", escapeAttr(r.Category)),
			fmt.Sprintf("score=%q", fmt.Sprintf("%.6f", hit.Score)),
			`origin="remote"`,
		}
		if p.Debug {
			attrs = append(attrs,
				`origins="remote"`,
				fmt.Sprintf("rrf_ranks=%q", federatedRankSummary(hit)),
			)
		}
		w.hit(attrs, r.Content)
	}
	return w.close(), nil
}

func remoteSearchMetadataAttrs(response *remotesync.RemoteSearchResponse) string {
	if response == nil {
		return ""
	}
	var attrs []string
	if response.RequestedMode != "" {
		attrs = append(attrs, fmt.Sprintf(
			"requested_mode=%q", escapeAttr(response.RequestedMode),
		))
	}
	if response.EffectiveMode != "" {
		attrs = append(attrs, fmt.Sprintf(
			"effective_mode=%q", escapeAttr(response.EffectiveMode),
		))
	}
	if response.Fallback {
		attrs = append(attrs, `fallback="text"`)
	}
	if response.FallbackReason != "" {
		attrs = append(attrs, fmt.Sprintf(
			"fallback_reason=%q", escapeAttr(response.FallbackReason),
		))
	}
	if len(attrs) == 0 {
		return ""
	}
	return " " + strings.Join(attrs, " ")
}

func federatedRankSummary(hit federatedSearchHit) string {
	ranks := make([]string, 0, 2)
	if hit.LocalRank > 0 {
		ranks = append(ranks, fmt.Sprintf("local:%d", hit.LocalRank))
	}
	if hit.RemoteRank > 0 {
		ranks = append(ranks, fmt.Sprintf("remote:%d", hit.RemoteRank))
	}
	return strings.Join(ranks, ",")
}

// resolveRemoteEntry maps an explicit remote selector to a config entry:
// "" / "_" / "default" resolve the cwd's default remote, anything else is a
// named entry from cfg.Remotes. Returns nil when nothing matches.
func (s *Server) resolveRemoteEntry(selector, cwd string) *config.RemoteEntry {
	if selector == "" || selector == "_" || selector == "default" {
		return s.cfg.ResolveRemote(cwd)
	}
	if e, ok := s.cfg.Remotes[selector]; ok {
		e.Name = selector
		return &e
	}
	return nil
}

// renderSearchResults emits the search hit list as compact XML so an LLM
// agent can integrate fragments directly into its reply without reformatting.
// Mirrors the structure of <anchored_context>/<anchored_search_preview>:
// attribute-level metadata + one entry per result. Score is rendered with
// three decimals to keep the snippet stable across runs (BM25/RRF jitter at
// the fourth decimal would create noisy diffs in tests).
func renderSearchResults(query string, globalMode bool, results []memory.SearchResult, debug, full bool) string {
	w := newSearchHitWriter(full)
	w.open("<anchored_search query=%q count=%q>\n",
		truncateRunes(query, 200), fmt.Sprintf("%d", len(results)),
	)
	for _, r := range results {
		var attrs []string
		attrs = append(attrs,
			fmt.Sprintf("id=%q", escapeAttr(r.Memory.ID)),
			fmt.Sprintf("category=%q", escapeAttr(r.Memory.Category)),
			fmt.Sprintf("score=%q", fmt.Sprintf("%.3f", r.Score)),
		)
		if globalMode && r.Memory.ProjectID != nil && *r.Memory.ProjectID != "" {
			attrs = append(attrs, fmt.Sprintf("project=%q", escapeAttr(*r.Memory.ProjectID)))
		}
		if debug && len(r.Signals) > 0 {
			attrs = append(attrs, fmt.Sprintf("signals=%q", escapeAttr(strings.Join(r.Signals, ","))))
		}
		w.hit(attrs, r.Memory.Content)
	}
	return w.close()
}
