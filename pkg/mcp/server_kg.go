package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

func (s *Server) toolKGQuery(ctx context.Context, args json.RawMessage) (string, error) {
	if s.kg == nil {
		return "Knowledge graph not available.", nil
	}

	var p struct {
		Entity string `json:"entity"`
		CWD    string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	var projectID *string
	if pid := s.mem.ResolveProject(s.rootedPath(ctx, p.CWD)); pid != "" {
		projectID = &pid
	}

	triples, err := s.kg.Query(ctx, p.Entity, projectID)
	if err != nil {
		return "", err
	}

	if len(triples) == 0 {
		return fmt.Sprintf("No relationships found for \"%s\".", p.Entity), nil
	}

	var lines []string
	for _, t := range triples {
		lines = append(lines, fmt.Sprintf("• %s — %s → %s", t.Subject, t.Predicate, t.Object))
	}

	return joinLines(lines), nil
}

// kgAddWriteBudget bounds how long kg_add waits to learn which remote owns the
// repository. A remote that has not answered by then holds the triple.
var kgAddWriteBudget = 5 * time.Second

func (s *Server) toolKGAdd(ctx context.Context, args json.RawMessage) (string, error) {
	if s.kg == nil {
		return "Knowledge graph not available.", nil
	}

	var p struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Object    string `json:"object"`
		CWD       string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Same convention as toolSave: an omitted cwd resolves to the session's
	// root so the triple lands project-scoped and the auto write-through
	// below can find the linked remote.
	p.CWD = s.rootedCWD(ctx, p.CWD)

	var projectID *string
	if pid := s.mem.ResolveProject(p.CWD); pid != "" {
		projectID = &pid
	}

	triple, err := s.kg.AddTriple(ctx, p.Subject, p.Predicate, p.Object, projectID)
	if err != nil {
		return "", err
	}

	result := fmt.Sprintf("Added relationship: %s — %s → %s (id: %s)", triple.Subject, triple.Predicate, triple.Object, triple.ID)

	// Auto write-through: mirror the just-added triple to the remote, same
	// local-first contract as toolSave — the local add already succeeded, so
	// a remote failure never affects the result. The target remote (and its
	// auto_sync gate) is resolved inside via the cross-remote origin probe.
	// The triple passes the same safety filter as a memory first; one it
	// blocks never asks a remote at all.
	if s.cfg != nil {
		if s.hasAnyRemote() {
			root := s.projectFilterRoot(ctx, projectID)
			if _, reason := remotesync.SafeTriple(remotesync.TripleFromKG(*triple), root); reason != "" {
				return result + " (local only — blocked by safety policy: " + reason + ")", nil
			}
			r := s.resolveAutoRemoteWriteTarget(ctx, p.CWD, kgAddWriteBudget)
			if target, pid := r.Entry, r.ProjectID; target != nil && pid != "" && target.AutoSyncEnabled() {
				go s.pushTripleTo(*target, pid, root, *triple)
				result += fmt.Sprintf(" (auto-sync → %s)", target.Name)
			} else if r.Held() {
				result += " (local only — " + r.HeldReason() + "; `anchored remote sync` sends it later)"
			} else {
				result += " (local only — no remote project matched this repo)"
			}
		}
	}

	return result, nil
}
