package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
	"github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/sync"
)

func runSave(args []string) {
	fs := newFlagSet("save")
	category := fs.String("category", "", "memory category (auto-detected if omitted)")
	project := fs.String("project", "", "project ID")
	configPath := fs.String("config", "", "path to config file")
	remoteFlag := fs.String("remote", "", "push to remote server (name or empty for default)")

	remoteExplicit := hasExplicitFlag(args, "remote")
	fs.Parse(reorderArgsForFlag(fs, args))

	content := strings.Join(fs.Args(), " ")
	if content == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored save <content> [--category] [--project] [--remote]")
		os.Exit(1)
	}

	cfg, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	// Pass the real working directory so memories saved from inside a project
	// get project scope (and so become eligible for team sync), matching the
	// MCP path. Without this, CLI saves defaulted to user scope and never synced.
	saveCWD, _ := os.Getwd()

	m, err := svc.Save(ctx, content, *category, "cli", saveCWD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "save error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Saved memory %s [%s]\n", m.ID, m.Category)

	if !remoteExplicit {
		// No explicit --remote: honor auto_sync (local-first write-through).
		autoSyncRemote(ctx, cfg, svc, m, *project)
		return
	}

	pushRemote(ctx, cfg, svc, m, *remoteFlag, *project)
}

// autoSyncRemote pushes a just-saved memory to the default remote when that
// remote has auto_sync enabled. Local save already succeeded; this is
// best-effort. The safety filter gates eligibility and redacts content, so
// user-scoped/personal/secret memories never leave the machine.
func autoSyncRemote(ctx context.Context, cfg *config.Config, svc *memory.Service, m *memory.Memory, projectOverride string) {
	cwd, _ := os.Getwd()
	entry := cfg.ResolveRemote(cwd)
	if entry == nil || !entry.AutoSyncEnabled() {
		return
	}
	redacted, ok := sync.ClassifyForAutoSync(*m, cwd)
	if !ok {
		return
	}
	// Routing: explicit override first, then the cross-remote origin probe —
	// every configured remote is asked whether it knows the repo's git-origin
	// key, so a freshly-configured second server works with zero extra
	// routing setup. The linked-project fallback only applies to non-repo
	// contexts. The local project id is meaningless to the server.
	projectID := projectOverride
	originRouted := false
	if projectID == "" {
		if proj, err := svc.ResolveProjectInfo(cwd); err == nil && proj != nil && proj.RemoteKey != "" {
			originRouted = true
			canonicalKey, legacyKey := project.RemoteKeysFromDir(proj.Path)
			r := sync.ResolveProjectAcrossRemotesForWrite(ctx, cfg, cwd, "cli", canonicalKey, legacyKey)
			if r.Held() {
				fmt.Fprintf(os.Stderr, "warning: auto-sync held: %s; run `anchored remote sync` once it answers\n", r.HeldReason())
				return
			}
			if target, pid := r.Entry, r.ProjectID; target != nil && pid != "" {
				if !target.AutoSyncEnabled() {
					return
				}
				entry = target
				projectID = pid
			}
		}
	}
	if projectID == "" && !originRouted && len(entry.Projects) > 0 {
		projectID = entry.Projects[0]
	}
	if projectID == "" {
		return
	}
	client := sync.NewClientFromEntry(*entry, "cli")
	mem := sync.RemoteMemory{ID: m.ID, Category: m.Category, Content: redacted, Source: m.Source, ProjectID: projectID}
	if _, err := client.SaveRemote(ctx, mem); err != nil {
		fmt.Fprintf(os.Stderr, "warning: auto-sync skipped: %v\n", err)
		return
	}
	fmt.Printf("Auto-synced to remote %s (%s) · project %s\n", entry.Name, entry.ServerURL, projectID)
}

func pushRemote(ctx context.Context, cfg *config.Config, svc *memory.Service, m *memory.Memory, remoteName, projectOverride string) {
	var entry *config.RemoteEntry
	if remoteName != "" {
		r, ok := cfg.Remotes[remoteName]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: remote %q not found in config, skipping remote save\n", remoteName)
			return
		}
		entry = &r
	} else {
		cwd, _ := os.Getwd()
		entry = cfg.ResolveRemote(cwd)
	}

	if entry == nil {
		fmt.Fprintln(os.Stderr, "warning: no remote configured, skipping remote save")
		return
	}

	client := sync.NewClientFromEntry(*entry, "cli")

	// Same routing precedence as auto-sync: explicit override, then the
	// repo's git-origin remote_key (against the explicitly chosen remote),
	// then a linked project (non-repo contexts only). The local project id
	// is meaningless to the server.
	projectID := projectOverride
	originRouted := false
	if projectID == "" {
		cwd, _ := os.Getwd()
		if proj, rErr := svc.ResolveProjectInfo(cwd); rErr == nil && proj != nil && proj.RemoteKey != "" {
			canonicalKey, legacyKey := project.RemoteKeysFromDir(proj.Path)
			projectID, _ = client.ResolveProjectIDByRemoteKeys(ctx, canonicalKey, legacyKey)
			originRouted = true
		}
	}
	if projectID == "" && !originRouted && len(entry.Projects) > 0 {
		projectID = entry.Projects[0]
	}
	if projectID == "" {
		fmt.Fprintln(os.Stderr, "warning: remote save skipped: no remote project resolved (create the project on the server, or link one with `anchored remote link <id>`)")
		return
	}

	mem := sync.RemoteMemory{
		ID:        m.ID,
		Category:  m.Category,
		Content:   m.Content,
		Source:    m.Source,
		ProjectID: projectID,
	}

	resp, err := client.SaveRemote(ctx, mem)
	if err != nil {
		if sync.IsRemoteUnavailable(err) {
			fmt.Fprintf(os.Stderr, "warning: remote save skipped: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "warning: remote save failed: %v\n", err)
		return
	}

	fmt.Printf("Saved to remote %s [%s]\n", entry.Name, resp.ID)
}

func hasExplicitFlag(args []string, name string) bool {
	prefixDash := "-" + name
	prefixEq := "-" + name + "="
	prefixDDash := "--" + name
	prefixDEq := "--" + name + "="
	for _, a := range args {
		if a == prefixDash || a == prefixDDash || strings.HasPrefix(a, prefixEq) || strings.HasPrefix(a, prefixDEq) {
			return true
		}
	}
	return false
}
