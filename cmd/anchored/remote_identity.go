package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/sync"
)

// policyHintLine renders a one-line advisory from the server's sync policy, or
// "" when the server returned none (older server, or capability-less request).
// Surfacing it after a push lets the user see why memories may be rejected
// (blocked categories) and the batch cap, without a separate call.
func policyHintLine(p *sync.PolicyHints) string {
	if p == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if len(p.BlockedCategories) > 0 {
		parts = append(parts, "blocked categories ["+strings.Join(p.BlockedCategories, " ")+"]")
	}
	if p.MaxMemoriesPerSync > 0 {
		parts = append(parts, fmt.Sprintf("max %d per sync", p.MaxMemoriesPerSync))
	}
	if len(parts) == 0 {
		return ""
	}
	return "server policy: " + strings.Join(parts, "; ")
}

// buildNoProjectError formats the actionable failure for a claim-based sync
// when no configured remote has a project matching the repository's keys. It
// names exactly what was tried so the user can fix the right side (panel
// Repository URL, explicit link, or connectivity).
func buildNoProjectError(origin, canonicalKey, legacyKey string, remotes []string) string {
	var b strings.Builder
	b.WriteString("No configured remote has a project for this repository.\n")
	fmt.Fprintf(&b, "  Origin:        %s\n", origin)
	keys := canonicalKey
	if legacyKey != "" && legacyKey != canonicalKey {
		keys += ", " + legacyKey + " (legacy)"
	}
	fmt.Fprintf(&b, "  Keys tried:    %s\n", keys)
	fmt.Fprintf(&b, "  Remotes probed: %s\n", strings.Join(remotes, ", "))
	b.WriteString("Fix one of:\n")
	fmt.Fprintf(&b, "  • create the project in the dashboard with Repository URL %s (Connect tab has the exact steps)\n", origin)
	b.WriteString("  • link an existing project: anchored remote link <slug> --remote <name>\n")
	b.WriteString("  • if the server should know this repo already, check connectivity: anchored doctor\n")
	return b.String()
}

// registeredElsewhere names the first remote, other than skip, that lists a
// project under one of the keys, or "". It only informs `remote status`;
// routing never asks those remotes.
func registeredElsewhere(ctx context.Context, cfg *config.Config, skip string, keys ...string) string {
	for _, name := range cfg.RemoteNames() {
		if name == skip {
			continue
		}
		if pid, _ := sync.NewClientFromEntry(cfg.Remotes[name], "cli").ResolveProjectIDByRemoteKeys(ctx, keys...); pid != "" {
			return name
		}
	}
	return ""
}

// repoIdentityLines reports, for `remote status`, whether any configured
// remote has a project registered for the repository at cwd — and, when the
// routed remote carries exactly one linked project, whether that link points
// at this repository or at a different one (the classic wrong-project push).
func repoIdentityLines(ctx context.Context, cfg *config.Config, cwd, origin, canonicalKey, legacyKey string) []string {
	if canonicalKey == "" && legacyKey == "" {
		return nil
	}

	var lines []string
	target, projectID, matchedKey := sync.ResolveProjectAcrossRemotes(ctx, cfg, cwd, "cli", canonicalKey, legacyKey)
	routed, pattern := cfg.ResolveRemoteWithPattern(cwd)
	switch {
	case target != nil && projectID != "":
		lines = append(lines, fmt.Sprintf("  Identity:   match — project %s on %q (key %s)", projectID, target.Name, matchedKey))
	case pattern != "":
		// An explicit glob limits reads and writes to its remote, so a
		// registration on another one is ignored: say so, it looks like a match.
		lines = append(lines, fmt.Sprintf("  Identity:   %q, the remote its glob %s routes to, has no project for this repo", routed.Name, pattern))
		if other := registeredElsewhere(ctx, cfg, routed.Name, canonicalKey, legacyKey); other != "" {
			lines = append(lines, fmt.Sprintf("  Warning:    the repo is registered on %q, but its glob routes reads and writes to %q only", other, routed.Name))
		}
		lines = append(lines, fmt.Sprintf("              → create it on %q with Repository URL %s, or change the glob", routed.Name, origin))
	default:
		lines = append(lines, "  Identity:   no configured remote has a project for this repo")
		lines = append(lines, fmt.Sprintf("              → create it in the dashboard with Repository URL %s, or: anchored remote link <slug> --remote <name>", origin))
	}

	// A single linked project on the routed remote is a hard expectation from
	// the user ("syncs from here go THERE") — verify it belongs to this repo.
	if entry := cfg.ResolveRemote(cwd); entry != nil && len(entry.Projects) == 1 {
		client := sync.NewClientFromEntry(*entry, "cli")
		if rp := client.GetProjectByID(ctx, entry.Projects[0]); rp != nil && (rp.RemoteKey != "" || rp.RemoteKeyV1 != "") {
			if remoteKeysMatch(rp, canonicalKey, legacyKey) {
				lines = append(lines, fmt.Sprintf("  Link check: linked project %q matches this repo", rp.Slug))
			} else {
				lines = append(lines, fmt.Sprintf("  Link check: MISMATCH — linked project %q is registered to a different repository", rp.Slug))
				lines = append(lines, "              → unlink it (anchored remote unlink) or fix its Repository URL in the dashboard")
			}
		}
	}
	return lines
}
