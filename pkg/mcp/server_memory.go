package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

func (s *Server) toolSave(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Content  string `json:"content"`
		Category string `json:"category"`
		CWD      string `json:"cwd"`
		Scope    string `json:"scope"`
		// Remote distinguishes absent (nil → auto write-through when the
		// resolved remote has auto_sync on) from explicitly set (sync save
		// to that remote; "" / "_" / "default" → default remote).
		Remote *string `json:"remote"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	p.CWD = s.sessionCWD(ctx, p.CWD)
	category, ok := memory.NormalizeCategory(p.Category)
	if !ok {
		return "", fmt.Errorf("unknown category %q: use one of %s", p.Category, strings.Join(memory.Categories, ", "))
	}
	p.Category = category

	var (
		target    *config.RemoteEntry
		projectID string
	)
	if s.cfg != nil && p.Remote != nil {
		target = s.resolveRemoteEntry(*p.Remote, p.CWD)
	}

	saveOpts := memory.SaveOptions{
		Content:         p.Content,
		Category:        p.Category,
		Source:          "mcp",
		CWD:             p.CWD,
		PreferenceScope: p.Scope,
	}
	outboxEligible := false
	autoSyncSafetyBlocked := false
	var m *memory.Memory
	var err error
	if p.Remote == nil {
		m, err = s.mem.SaveDurable(ctx, memory.DurableSaveOptions{
			SaveOptions: saveOpts,
			DeriveRemoteOutbox: func(m memory.Memory) ([]memory.RemoteOutboxSpec, error) {
				if !s.hasAutoSyncRemote() {
					return nil, nil
				}
				content, _, ok := remotesync.ClassifyForRemote(m, s.projectFilterRoot(ctx, m.ProjectID))
				if !ok {
					autoSyncSafetyBlocked = true
					return nil, nil
				}
				payload, err := json.Marshal(remoteOutboxEnvelope{
					Kind: autoRouteOutboxKind,
					// Absolute: the row may be delivered by a process that
					// runs in another directory.
					CWD: absCWD(p.CWD),
					Memory: remotesync.RemoteMemory{
						ID: m.ID, Category: m.Category, Content: content,
						Source: "mcp",
					},
				})
				if err != nil {
					return nil, err
				}
				outboxEligible = true
				return []memory.RemoteOutboxSpec{{
					Remote:  autoRouteOutboxKind,
					Payload: payload,
				}}, nil
			},
		})
	} else {
		// Explicit remote saves retain the legacy synchronous contract. The
		// durable outbox is reserved for implicit auto-sync.
		m, err = s.mem.SaveWithOptions(ctx, saveOpts)
	}
	if err != nil {
		return "", err
	}

	result := fmt.Sprintf("Saved [%s] memory %s", m.Category, m.ID)

	if p.Remote != nil && s.cfg != nil {
		// Preserve the local-first contract for explicit remote saves: the
		// SQLite commit above must complete before any network lookup can
		// block, fail, or observe cancellation. The classifier runs first,
		// so a memory it keeps local makes no network call at all.
		content, reason, sendable := remotesync.ClassifyForNamedRemote(*m, s.projectFilterRoot(ctx, m.ProjectID))
		if sendable && target != nil {
			client := remotesync.NewClientFromEntry(*target, "mcp")
			projectID, _ = s.resolveRemoteProjectID(ctx, client, *target, p.CWD)
		}
		switch {
		case !sendable:
			result += fmt.Sprintf(" (remote: blocked by safety policy: %s, local save preserved)", reason)
		case target == nil:
			result += " (remote: no remote configured, skipped)"
		case projectID == "":
			result += " (remote: no remote project resolved, skipped — link one with `anchored remote link <id>` or create it on the server)"
		default:
			remoteMemory := remotesync.RemoteMemory{
				ID: m.ID, Category: m.Category, Content: content,
				Source: "mcp", ProjectID: projectID,
			}
			payload, marshalErr := json.Marshal(remoteMemory)
			if marshalErr != nil {
				return "", marshalErr
			}
			sum := sha256.Sum256([]byte(
				remoteOutboxName(*target) + "\x00" + projectID + "\x00" +
					m.ID + "\x00" + string(payload),
			))
			client := remotesync.NewClientFromEntry(*target, "mcp")
			response, remoteErr := client.SaveRemoteIdempotent(
				ctx, remoteMemory, hex.EncodeToString(sum[:]),
			)
			if remoteErr != nil {
				if remotesync.IsRemoteForbidden(remoteErr) || remotesync.IsRemoteUnavailable(remoteErr) {
					result += " (remote: unavailable, local save preserved)"
				} else {
					result += fmt.Sprintf(" (remote: %v)", remoteErr)
				}
				break
			}
			result += fmt.Sprintf(" (remote: saved to %s)", target.Name)
			if !response.Created {
				result += " [updated existing]"
			}
		}
	} else if p.Remote == nil && s.cfg != nil {
		switch {
		case outboxEligible:
			result += " (auto-sync queued)"
		case autoSyncSafetyBlocked:
			result += " (local only — blocked by safety policy)"
		case s.hasAnyRemote() && !s.hasAutoSyncRemote():
			result += " (local only — auto_sync disabled)"
		}
	}

	return result, nil
}

func remoteOutboxName(entry config.RemoteEntry) string {
	// The config map key/name is user-editable. Persist the endpoint identity
	// so queued envelopes still resolve after a harmless remote rename.
	if entry.ServerURL != "" {
		return entry.ServerURL
	}
	return entry.Name
}

func (s *Server) toolList(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		CWD      string `json:"cwd"`
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID string
	if cwd := s.sessionPath(ctx, p.CWD); cwd != "" {
		projectID = s.mem.ResolveProject(cwd)
	}

	memories, err := s.mem.List(ctx, memory.ListOptions{
		Category:  p.Category,
		Limit:     p.Limit,
		ProjectID: projectID,
	})
	if err != nil {
		return "", err
	}

	if len(memories) == 0 {
		return "No memories found.", nil
	}

	var lines []string
	for i, m := range memories {
		// One flattened, capped line per memory — a page of long multi-line
		// memories would otherwise blow the response budget (the full text
		// of one memory is readable via anchored_search full=true).
		content := strings.ReplaceAll(m.Content, "\n", " ")
		content = strings.ReplaceAll(content, "\r", " ")
		lines = append(lines, fmt.Sprintf("%d. [%s] %s — %s",
			i+1, m.Category, m.CreatedAt.Format("2006-01-02 15:04"), truncateRunes(content, listItemRunes)))
	}

	return fmt.Sprintf("Showing %d memories:\n\n%s", len(memories), joinLines(lines)), nil
}

func (s *Server) toolForget(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID   string `json:"id"`
		Hard bool   `json:"hard"`
		CWD  string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if p.Hard {
		if err := s.mem.Forget(ctx, p.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Permanently deleted memory %s", p.ID), nil
	}

	if err := s.mem.SoftForget(ctx, p.ID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Soft-deleted memory %s", p.ID), nil
}

func (s *Server) toolUpdate(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		ID       string `json:"id"`
		Content  string `json:"content"`
		Category string `json:"category"`
		CWD      string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	m, err := s.mem.Update(ctx, p.ID, p.Content, p.Category)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("Updated [%s] memory %s", m.Category, m.ID), nil
}

func (s *Server) toolStats(ctx context.Context) (string, error) {
	stats, err := s.mem.Stats(ctx)
	if err != nil {
		return "", err
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Total memories: %d", stats.TotalMemories))

	if len(stats.ByCategory) > 0 {
		lines = append(lines, "\nBy category:")
		for cat, count := range stats.ByCategory {
			lines = append(lines, fmt.Sprintf("  %s: %d", cat, count))
		}
	}

	if len(stats.ByProject) > 0 {
		lines = append(lines, "\nBy project:")
		for proj, count := range stats.ByProject {
			lines = append(lines, fmt.Sprintf("  %s: %d", proj, count))
		}
	}

	if s.sessions != nil {
		total, active, err := s.sessions.SessionStats(ctx)
		if err == nil {
			lines = append(lines, fmt.Sprintf("\nSessions: %d total, %d active", total, active))
		}
	}

	return joinLines(lines), nil
}

func (s *Server) toolSessionEnd(ctx context.Context, args json.RawMessage) (string, error) {
	if s.sessions == nil {
		return "Session tracking not available.", nil
	}

	var p struct {
		SessionID string `json:"session_id"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if p.SessionID == "" {
		return "", fmt.Errorf("session_id is required")
	}

	if err := s.sessions.EndSession(ctx, p.SessionID); err != nil {
		return "", err
	}

	if p.Summary != "" {
		_, err := s.mem.Save(ctx, p.Summary, "summary", "session_end", ".")
		if err != nil {
			return fmt.Sprintf("Session %s ended (summary save failed: %v)", p.SessionID, err), nil
		}
		return fmt.Sprintf("Session %s ended with summary saved.", p.SessionID), nil
	}

	return fmt.Sprintf("Session %s ended.", p.SessionID), nil
}
