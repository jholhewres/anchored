package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/memory"
	projectpkg "github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/sync"
)

func runRemote(args []string) {
	if len(args) == 0 {
		printRemoteUsage()
		return
	}
	switch args[0] {
	case "configure":
		runRemoteConfigure(args[1:])
	case "link":
		runRemoteLink(args[1:])
	case "unlink":
		runRemoteUnlink(args[1:])
	case "status":
		runRemoteStatus(args[1:])
	case "preview":
		runRemotePreview(args[1:])
	case "sync":
		runRemoteSync(args[1:])
	case "sync-per-project":
		runRemoteSyncPerProject(args[1:])
	default:
		printRemoteUsage()
	}
}

func printRemoteUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  anchored remote configure --server URL --key KEY   Wire a remote anchored_oss server (--name for a 2nd server)")
	fmt.Fprintln(os.Stderr, "  anchored remote link <project_id> [--remote NAME]   Subscribe to a remote project so its memories sync")
	fmt.Fprintln(os.Stderr, "  anchored remote unlink <project_id> [--remote NAME] Stop syncing memories tied to a remote project")
	fmt.Fprintln(os.Stderr, "  anchored remote status                              Show remote config + how the current repo routes (git origin)")
	fmt.Fprintln(os.Stderr, "  anchored remote preview                             Preview which memories would sync (offline)")
	fmt.Fprintln(os.Stderr, "  anchored remote sync [--remote NAME]                Sync the CURRENT repo by its git origin (--dry-run to preview)")
	fmt.Fprintln(os.Stderr, "  anchored remote sync --all                          Sync every local project, each routed by its own git origin")
	fmt.Fprintln(os.Stderr, "  anchored remote sync --project-id <id>              Force all current-repo memories into a specific remote project")
}

// runRemoteLink adds a remote project_id to a remote's linked-projects list.
// Project IDs are server-scoped, so the link is stored on the specific remote
// it belongs to: --remote <name> targets a named entry, the default is the
// legacy singular block. Idempotent.
func runRemoteLink(args []string) {
	fs := newFlagSet("remote link")
	remoteName := fs.String("remote", "", "named remote this project belongs to (default: the default remote)")
	fs.Parse(reorderArgsForFlag(fs, args))
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Usage: anchored remote link <project_id> [--remote <name>]")
		os.Exit(1)
	}
	projectID := fs.Arg(0)

	configFile, cfg := loadWritableConfig()

	if *remoteName != "" && *remoteName != "default" {
		entry, ok := cfg.Remotes[*remoteName]
		if !ok {
			fmt.Fprintf(os.Stderr, "remote %q not found in config (available: %s)\n", *remoteName, remoteNames(cfg))
			os.Exit(1)
		}
		entry.Name = *remoteName
		resolved, err := resolveLinkTarget(projectID, entry, *remoteName)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		projectID = resolved
		for _, p := range entry.Projects {
			if p == projectID {
				fmt.Printf("Already linked to %s on remote %q\n", projectID, *remoteName)
				return
			}
		}
		entry.Projects = append(entry.Projects, projectID)
		cfg.Remotes[*remoteName] = entry
		writeConfigFile(configFile, cfg)
		fmt.Printf("Linked project %s to remote %q (%s)\n", projectID, *remoteName, entry.ServerURL)
		fmt.Printf("  Projects linked to this remote: %d\n", len(entry.Projects))
		return
	}

	resolved, err := resolveLinkTarget(projectID, config.RemoteEntry{
		Name:      "default",
		ServerURL: cfg.Remote.ServerURL,
		APIKey:    cfg.Remote.APIKey,
	}, "default")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	projectID = resolved

	for _, p := range cfg.Remote.Projects {
		if p == projectID {
			fmt.Printf("Already linked to %s\n", projectID)
			return
		}
	}
	cfg.Remote.Projects = append(cfg.Remote.Projects, projectID)
	writeConfigFile(configFile, cfg)
	fmt.Printf("Linked project %s to the default remote (%s)\n", projectID, orEmpty(cfg.Remote.ServerURL, "not configured yet"))
	fmt.Printf("  Total projects subscribed: %d\n", len(cfg.Remote.Projects))
}

// uuidRe matches a canonical project UUID. Anything that doesn't match is
// treated as a project slug to look up on the target remote.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// resolveLinkTarget returns the project ID to link. When arg already looks
// like a UUID it is returned unchanged. Otherwise arg is treated as a slug and
// matched (exactly) against the projects on the target remote; an unknown slug
// returns an error that lists the available slugs.
func resolveLinkTarget(arg string, entry config.RemoteEntry, remoteName string) (string, error) {
	if uuidRe.MatchString(arg) {
		return arg, nil
	}
	if entry.ServerURL == "" {
		return "", fmt.Errorf("cannot resolve slug %q: remote %q has no server URL configured", arg, remoteName)
	}
	client := sync.NewClientFromEntry(entry, "cli")
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		return "", fmt.Errorf("could not list projects on remote %q: %w", remoteName, err)
	}
	for _, p := range projects {
		if p.Slug == arg {
			return p.ID, nil
		}
	}
	slugs := make([]string, 0, len(projects))
	for _, p := range projects {
		if p.Slug != "" {
			slugs = append(slugs, p.Slug)
		}
	}
	sort.Strings(slugs)
	available := "(none)"
	if len(slugs) > 0 {
		available = strings.Join(slugs, ", ")
	}
	return "", fmt.Errorf("no project with slug %q on remote %q\navailable slugs: %s", arg, remoteName, available)
}

// runRemoteUnlink removes a project_id from a remote's linked-projects list.
// No-op if the id isn't present. --remote <name> targets a named entry.
func runRemoteUnlink(args []string) {
	fs := newFlagSet("remote unlink")
	remoteName := fs.String("remote", "", "named remote the link lives on (default: the default remote)")
	fs.Parse(reorderArgsForFlag(fs, args))
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "Usage: anchored remote unlink <project_id> [--remote <name>]")
		os.Exit(1)
	}
	projectID := fs.Arg(0)

	configFile, cfg := loadWritableConfig()

	if *remoteName != "" && *remoteName != "default" {
		entry, ok := cfg.Remotes[*remoteName]
		if !ok {
			fmt.Fprintf(os.Stderr, "remote %q not found in config (available: %s)\n", *remoteName, remoteNames(cfg))
			os.Exit(1)
		}
		out := entry.Projects[:0]
		removed := false
		for _, p := range entry.Projects {
			if p == projectID {
				removed = true
				continue
			}
			out = append(out, p)
		}
		entry.Projects = out
		cfg.Remotes[*remoteName] = entry
		writeConfigFile(configFile, cfg)
		if removed {
			fmt.Printf("Unlinked project %s from remote %q\n", projectID, *remoteName)
		} else {
			fmt.Printf("Project %s was not linked on remote %q\n", projectID, *remoteName)
		}
		return
	}

	out := cfg.Remote.Projects[:0]
	removed := false
	for _, p := range cfg.Remote.Projects {
		if p == projectID {
			removed = true
			continue
		}
		out = append(out, p)
	}
	cfg.Remote.Projects = out
	writeConfigFile(configFile, cfg)
	if removed {
		fmt.Printf("Unlinked project %s\n", projectID)
	} else {
		fmt.Printf("Project %s was not linked\n", projectID)
	}
}

// runRemoteConfigure wires (or rewires) a remote anchored_oss server into the
// local config. It always sets remote.enabled=true unless --disable is passed.
// Existing values are overwritten — config rotation is via re-running this.
func runRemoteConfigure(args []string) {
	fs := newFlagSet("remote configure")
	server := fs.String("server", "", "remote server URL (e.g. https://anchored.acme.com)")
	key := fs.String("key", "", "admin/sync API key for the server")
	name := fs.String("name", "", "name for this remote (e.g. company). Omitted: updates the default, or auto-names default-2, default-3… when a different default server already exists")
	disable := fs.Bool("disable", false, "turn remote sync off (other flags are ignored)")
	fs.Parse(args)

	configFile, cfg := loadWritableConfig()

	if *disable {
		cfg.Remote.Enabled = false
		writeConfigFile(configFile, cfg)
		fmt.Printf("Remote sync disabled (config: %s)\n", configFile)
		return
	}

	if *server == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "Usage: anchored remote configure --server URL --key KEY [--name NAME] [--disable]")
		os.Exit(1)
	}

	newURL := strings.TrimRight(*server, "/")
	target := *name

	// No name: update the default in place when it's unset or already points
	// at this server. When the default points somewhere ELSE, keep it and
	// auto-name the new server (default-2, default-3, …) instead of silently
	// overwriting — users running configure twice for two servers (personal +
	// company) end up with both.
	if target == "" || target == "default" {
		if target == "default" || cfg.Remote.ServerURL == "" || cfg.Remote.ServerURL == newURL {
			// Project IDs are server-scoped, so pointing at a different
			// server makes the existing links meaningless — clear them to
			// avoid stale "project not found" pushes.
			if cfg.Remote.ServerURL != "" && cfg.Remote.ServerURL != newURL && len(cfg.Remote.Projects) > 0 {
				fmt.Printf("Server changed (%s → %s); cleared %d stale project link(s).\n",
					cfg.Remote.ServerURL, newURL, len(cfg.Remote.Projects))
				cfg.Remote.Projects = nil
			}
			cfg.Remote.Enabled = true
			cfg.Remote.ServerURL = newURL
			cfg.Remote.APIKey = *key
			writeConfigFile(configFile, cfg)
			fmt.Printf("Remote sync configured.\n")
			fmt.Printf("  Name:   default\n")
			fmt.Printf("  Server: %s\n", cfg.Remote.ServerURL)
			fmt.Printf("  Key:    %s\n", maskKey(cfg.Remote.APIKey))
			fmt.Printf("  Config: %s\n", configFile)
			return
		}
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("default-%d", n)
			if _, ok := cfg.Remotes[candidate]; !ok {
				target = candidate
				break
			}
		}
		fmt.Printf("The default remote already points at %s — adding this server as %q.\n", cfg.Remote.ServerURL, target)
		fmt.Printf("Tip: give it a meaningful name with --name (or rename it under remotes: in the config).\n")
	}

	if cfg.Remotes == nil {
		cfg.Remotes = map[string]config.RemoteEntry{}
	}
	entry := cfg.Remotes[target]
	if entry.ServerURL != "" && entry.ServerURL != newURL && len(entry.Projects) > 0 {
		fmt.Printf("Server changed for %q (%s → %s); cleared %d stale project link(s).\n",
			target, entry.ServerURL, newURL, len(entry.Projects))
		entry.Projects = nil
	}
	entry.ServerURL = newURL
	entry.APIKey = *key
	// First server configured under a name (the Connect-tab flow) must become
	// the default: with no default and no routing paths, ResolveRemote finds
	// nothing and the MCP auto-sync/search-merge gates silently stay closed —
	// the config LOOKS right but nothing reaches the server.
	if cfg.Remote.ServerURL == "" && !entry.Default {
		hasDefault := false
		for n, e := range cfg.Remotes {
			if n != target && e.Default {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			entry.Default = true
			fmt.Printf("Marked %q as the default remote (first server configured).\n", target)
		}
	}
	cfg.Remotes[target] = entry

	writeConfigFile(configFile, cfg)
	fmt.Printf("Remote %q configured.\n", target)
	fmt.Printf("  Server: %s\n", entry.ServerURL)
	fmt.Printf("  Key:    %s\n", maskKey(entry.APIKey))
	fmt.Printf("  Config: %s\n", configFile)
	fmt.Printf("\nRoute repos to it with path globs (remotes.%s.paths) or use --remote %s on sync/link.\n", target, target)
}

func runRemoteStatus(args []string) {
	fs := newFlagSet("remote status")
	configPath := fs.String("config", "", "path to config file")
	fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	fmt.Printf("Remote sync: %s\n", boolStr(cfg.Remote.Enabled, "enabled", "disabled"))
	// Single-server setups keep the familiar detailed header; with named
	// remotes the map view below already lists the default entry, so the
	// header would just duplicate it.
	if len(cfg.Remotes) <= 1 {
		fmt.Printf("Server URL:  %s\n", orEmpty(cfg.Remote.ServerURL, "(not configured)"))
		fmt.Printf("API Key:     %s\n", maskKey(cfg.Remote.APIKey))
		fmt.Printf("Projects:    %d configured\n", len(cfg.Remote.Projects))
	}

	// Multi-server view: loadConfig migrates the singular block into the
	// named map, so this lists every server (default included) with its
	// own routing paths and per-server project links.
	if len(cfg.Remotes) > 1 {
		fmt.Println("\nConfigured remotes:")
		names := make([]string, 0, len(cfg.Remotes))
		for name := range cfg.Remotes {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			e := cfg.Remotes[name]
			marker := ""
			if e.Default {
				marker = " (default)"
			}
			fmt.Printf("  %-12s %s%s\n", name, e.ServerURL, marker)
			if len(e.Paths) > 0 {
				fmt.Printf("  %-12s   paths: %s\n", "", strings.Join(e.Paths, ", "))
			}
			fmt.Printf("  %-12s   linked projects: %d\n", "", len(e.Projects))
		}
	}

	// Show how the current repo would route, so the user can confirm the
	// git-origin match before syncing. Best-effort: needs an initialized store.
	if _, _, svc, err := initService(*configPath); err == nil {
		defer svc.Close()
		cwd, _ := os.Getwd()
		if proj, err := svc.ResolveProjectInfo(cwd); err == nil && proj != nil {
			fmt.Println()
			fmt.Printf("Current repo: %s\n", proj.Name)
			fmt.Printf("  Origin:     %s\n", orEmpty(gitOriginURL(cwd), "(no origin — sync will refuse)"))
			fmt.Printf("  Remote key: %s\n", orEmpty(proj.RemoteKey, "(none)"))
			// The conclusion users actually need: which server a sync/save
			// from this repo would talk to (path match, then default).
			if entry := cfg.ResolveRemote(cwd); entry != nil {
				how := "default remote"
				for _, pattern := range entry.Paths {
					if matched, _ := path.Match(pattern, cwd); matched {
						how = "path match: " + pattern
						break
					}
				}
				fmt.Printf("  Routes to:  %s (%s) — %s\n", entry.Name, entry.ServerURL, how)
			} else {
				fmt.Println("  Routes to:  (no remote configured)")
			}

			// Identity verification: does any remote actually have a project
			// registered for this repo, and does a single linked project (if
			// any) belong to it? Best-effort network probe, bounded.
			if len(cfg.Remotes) > 0 {
				canonicalKey, legacyKey := projectpkg.RemoteKeysFromDir(cwd)
				probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
				defer cancel()
				for _, line := range repoIdentityLines(probeCtx, cfg, cwd, gitOriginURL(cwd), canonicalKey, legacyKey) {
					fmt.Println(line)
				}
			}
		}
	}
}

func runRemotePreview(args []string) {
	fs := newFlagSet("remote preview")
	configPath := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project path filter (default: cwd)")
	format := fs.String("format", "table", "output format: table or json")
	fs.Parse(args)

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	projectRoot := *project
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}

	memories, err := listAllMemories(ctx, svc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list error: %v\n", err)
		os.Exit(1)
	}

	syncMemories := toSyncMemories(memories)

	result := sync.ClassifyForPreview(syncMemories, projectRoot)

	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(os.Stderr, "json encode error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Printf("Total: %d | Syncable: %d | Blocked: %d | Needs Review: %d\n\n",
			result.Total, result.Syncable, result.Blocked, result.NeedsReview)
		for _, item := range result.Items {
			content := item.Memory.Content
			if len(content) > 80 {
				content = content[:80] + "..."
			}
			fmt.Printf("  %-12s %-8s %s\n", item.Classification, item.Memory.Category, content)
			if item.Reason != "" {
				fmt.Printf("  %12s └─ %s\n", "", item.Reason)
			}
		}
	}
}

func boolStr(v bool, t, f string) string {
	if v {
		return t
	}
	return f
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func maskKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

// runRemoteSync syncs the CURRENT repository's memories to the remote server,
// resolveLinkedTarget decides the remote project to push to when git-origin
// routing didn't win and a project is linked to the remote (`anchored remote
// link <id>`). proj is the cwd's local project, or nil when the cwd is not a
// git repository.
//
//   - proj != nil → scope the push to that local project and funnel it into the
//     linked remote project; the downstream listing bounds it to proj.ID.
//   - proj == nil → there is nothing to bound the push, so the ENTIRE local
//     store would land in the linked project. Require an explicit --full-store
//     as confirmation; otherwise refuse with guidance. (Real incident: a dev
//     ran sync from a non-repo directory and pushed thousands of unrelated
//     memories into the team project.)
func resolveLinkedTarget(proj *projectpkg.Project, linkedID string, fullStore bool) (string, error) {
	if proj != nil {
		return linkedID, nil
	}
	if !fullStore {
		return "", fmt.Errorf(
			"Not inside a known project — refusing to push the entire local store to linked project %s.\n"+
				"Run `anchored remote sync` from inside the repository you want to sync (the common case),\n"+
				"or pass --full-store to push the ENTIRE local store to the linked project on purpose.\n"+
				"(If you ARE inside a repo, check that git is installed and `git rev-parse --show-toplevel` works here.)",
			linkedID,
		)
	}
	return linkedID, nil
}

// identifying the remote project by the repo's git-origin remote_key (not its
// local directory name). Two clones of the same repo — in different folders, on
// different machines — therefore land in the same remote project.
//
// Routing:
//   - default (repo-scoped): resolve the cwd's local project, push only its
//     memories, and send a ProjectClaim{Name, RemoteKey} so the server
//     resolves-or-creates the matching remote project by git origin.
//   - --project-id <id>: force all (cwd-scoped) memories into a specific remote
//     project (legacy/manual override).
//   - --all: push every local project, each routed by its own git-origin
//     remote_key (multi-project; see runRemoteSyncPerProject).
func runRemoteSync(args []string) {
	fs := newFlagSet("remote sync")
	configPath := fs.String("config", "", "path to config file")
	project := fs.String("project", "", "project path filter (default: cwd)")
	dryRun := fs.Bool("dry-run", false, "preview what would be pushed without making network calls")
	projectID := fs.String("project-id", "", "force a specific remote project ID (overrides git-origin routing)")
	remoteName := fs.String("remote", "", "use a specific named remote (default: resolve by project path, then the default remote)")
	all := fs.Bool("all", false, "sync every local project (each routed by its own git origin), not just the current repo")
	fullStore := fs.Bool("full-store", false, "with --project-id outside a repo: push the ENTIRE local store to that project (dangerous, off by default)")
	force := fs.Bool("force", false, "push even when the target project is registered to a different repository")
	fs.Parse(args)

	if *all {
		runRemoteSyncPerProject(args)
		return
	}

	cfg, logger, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	projectRoot := *project
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}

	proj, err := svc.ResolveProjectInfo(projectRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve project error: %v\n", err)
		os.Exit(1)
	}

	// Multi-server: pick the remote the same way search/save do — an explicit
	// --remote name wins, then the project-path match from the named remotes
	// map, then the default entry. Each repo can therefore sync to a
	// different server (e.g. personal vs company) from one config.
	var entry *config.RemoteEntry
	if *remoteName != "" {
		if e, ok := cfg.Remotes[*remoteName]; ok {
			e.Name = *remoteName
			entry = &e
		} else {
			fmt.Fprintf(os.Stderr, "remote %q not found in config (available: %s)\n", *remoteName, remoteNames(cfg))
			os.Exit(1)
		}
	} else {
		entry = cfg.ResolveRemote(projectRoot)
	}
	var entryProjects []string
	if entry != nil {
		entryProjects = entry.Projects
	}

	// When forcing a remote project id, the git-origin guard is bypassed
	// intentionally (manual override). Otherwise we require a git repo with an
	// origin so we can confirm the repository identity before pushing.
	if *projectID == "" {
		// Routing precedence: the repo's git-origin identity wins. A linked
		// project (`anchored remote link <id>`) is a global, not per-repo,
		// setting — letting it override origin routing silently funnels every
		// repo's memories (and KG triples) into one remote project. It is
		// only used as a fallback when the cwd has no matchable git origin.
		if proj != nil && proj.RemoteKey != "" {
			if len(entryProjects) > 0 {
				fmt.Printf("Repo has a git origin — routing by origin (ignoring linked project %s; use --project-id to force one)\n", entryProjects[0])
			}
		} else if len(entryProjects) > 0 {
			// A linked project is the routing fallback when git-origin routing
			// didn't win. resolveLinkedTarget enforces the safety rules (see
			// its doc); here we only translate its decision into output.
			pid, lerr := resolveLinkedTarget(proj, entryProjects[0], *fullStore)
			if lerr != nil {
				fmt.Fprintln(os.Stderr, lerr.Error())
				os.Exit(1)
			}
			*projectID = pid
			if proj == nil {
				fmt.Printf("Pushing entire local store to linked project %s (--full-store)\n", pid)
			} else {
				fmt.Printf("Using linked project %s (use --project-id to override)\n", pid)
			}
		} else {
			if proj == nil {
				fmt.Fprintln(os.Stderr, "Not inside a git repository — `remote sync` is repo-scoped.")
				fmt.Fprintln(os.Stderr, "Run it from a repo, use --all to sync every local project, or --project-id <id> to target one explicitly.")
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Repository %q has no git remote 'origin', so it can't be matched across machines.\n", proj.Name)
			fmt.Fprintln(os.Stderr, "Add one (git remote add origin <url>) or use --project-id <id> to target a remote project explicitly.")
			os.Exit(1)
		}
	}

	// Repo-scoped: pull only this project's memories. The full local store is
	// only ever pushed when the user asked for it in so many words
	// (--project-id + --full-store outside a repo) — it is never a fallback.
	var memories []memory.Memory
	if proj != nil {
		memories, err = listProjectMemories(ctx, svc, proj.ID)
	} else if *fullStore {
		memories, err = listAllMemories(ctx, svc)
	} else {
		fmt.Fprintln(os.Stderr, "Refusing to push the entire local store: --project-id outside a repository would send every memory you have to that project.")
		fmt.Fprintln(os.Stderr, "Run the sync inside the repository instead, or pass --full-store if pushing everything is really the intent.")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "list error: %v\n", err)
		os.Exit(1)
	}

	syncMemories := toSyncMemories(memories)
	preview := sync.ClassifyForPreview(syncMemories, projectRoot)

	if *dryRun {
		fmt.Println("=== DRY RUN: no network calls ===")
		if proj != nil {
			fmt.Printf("Repo:   %s\n", proj.Name)
			fmt.Printf("Origin: %s\n", orEmpty(gitOriginURL(projectRoot), "(none)"))
			fmt.Printf("Key:    %s\n", orEmpty(proj.RemoteKey, "(none)"))
		}
		fmt.Printf("Total: %d | Syncable: %d | Blocked: %d | Needs Review: %d\n\n",
			preview.Total, preview.Syncable, preview.Blocked, preview.NeedsReview)
		for _, item := range preview.Items {
			content := item.Memory.Content
			if len(content) > 80 {
				content = content[:80] + "..."
			}
			fmt.Printf("  %-12s %-8s %s\n", item.Classification, item.Memory.Category, content)
			if item.Reason != "" {
				fmt.Printf("  %12s └─ %s\n", "", item.Reason)
			}
		}
		return
	}

	// Compute both the canonical (v2) and legacy (v1) git-origin keys so every
	// resolution probes canonical first, then legacy — a project still
	// registered under the old normalization on a not-yet-rekeyed server is
	// still found. canonicalKey mirrors proj.RemoteKey (the stored canonical).
	canonicalKey, legacyKey := projectpkg.RemoteKeysFromDir(projectRoot)

	// Cross-remote origin probe: when no remote was forced and the resolved
	// one doesn't know this repository, ask the other configured remotes —
	// a freshly-configured second server (no routing paths yet) is found
	// automatically instead of the push failing with "project not found".
	if *remoteName == "" && proj != nil && proj.RemoteKey != "" && len(cfg.Remotes) > 1 {
		if target, _, _ := sync.ResolveProjectAcrossRemotes(ctx, cfg, projectRoot, "cli", canonicalKey, legacyKey); target != nil && (entry == nil || target.Name != entry.Name) {
			fmt.Printf("Repository is registered on remote %q — using it (force another with --remote)\n", target.Name)
			entry = target
		}
	}

	// The Enabled flag only exists on the legacy singular `remote:` block —
	// honor it when that block is what resolved (named `remotes:` entries
	// are enabled by definition; remove the entry to disable it).
	if entry != nil && entry.Name == "default" && cfg.Remote.ServerURL != "" && !cfg.Remote.Enabled {
		fmt.Fprintln(os.Stderr, "Remote sync is disabled. Enable in config or use --dry-run.")
		os.Exit(1)
	}
	if entry == nil {
		fmt.Fprintln(os.Stderr, "No remote configured. Run `anchored remote configure --server <url> --key <key>` or add a `remotes:` entry to the config.")
		os.Exit(1)
	}

	if len(cfg.Remotes) > 1 || *remoteName != "" {
		fmt.Printf("Remote: %s (%s)\n", entry.Name, entry.ServerURL)
	}
	client := sync.NewClientFromEntry(*entry, "cli")

	// Targeted pushes (linked project or --project-id) must land in a project
	// that is registered to THIS repository: when both sides carry routing
	// keys and none of them line up, the target belongs to a different repo
	// and the push would pollute it. --force is the documented escape hatch.
	if *projectID != "" && proj != nil && (canonicalKey != "" || legacyKey != "") {
		if rp := client.GetProjectByID(ctx, *projectID); rp != nil && (rp.RemoteKey != "" || rp.RemoteKeyV1 != "") {
			if !remoteKeysMatch(rp, canonicalKey, legacyKey) {
				if !*force {
					fmt.Fprintf(os.Stderr, "Target project %q (%s) is registered to a different repository (its routing key doesn't match this repo's origin).\n", rp.Slug, *projectID)
					fmt.Fprintln(os.Stderr, "Sync from the matching repository, fix the project's Repository URL in the dashboard, or pass --force to push anyway.")
					os.Exit(1)
				}
				fmt.Fprintf(os.Stderr, "warning: pushing to project %q even though its routing key doesn't match this repository (--force)\n", rp.Slug)
			}
		}
	}

	// Determine which git-origin key the chosen remote actually knows the repo
	// under: probe canonical first, then legacy. When the server has the
	// project under the legacy key, the whole outgoing payload (project_claim
	// AND every memory's remote_project_key) must use that legacy key so the
	// push lands in the existing project instead of creating a canonical
	// duplicate. Default to canonical when nothing resolves (new-project case,
	// which the server accepts/creates).
	pushKey := canonicalKey
	if *projectID == "" && proj != nil && proj.RemoteKey != "" {
		if _, matched := client.ResolveProjectIDByRemoteKeys(ctx, canonicalKey, legacyKey); matched != "" {
			pushKey = matched
		} else {
			// Pre-flight: a claim-based push for an unknown key fails on the
			// server with a bare "no project with remote_key" per memory.
			// Stop here with the actionable version instead. The cross-remote
			// probe above already widened the search when it could.
			fmt.Fprint(os.Stderr, buildNoProjectError(
				gitOriginURL(projectRoot), canonicalKey, legacyKey, sortedRemoteNames(cfg)))
			os.Exit(1)
		}
	}

	pushMemories := make([]sync.SyncMemory, 0, preview.Syncable)
	for _, item := range preview.Items {
		if item.Classification != sync.ClassificationSyncable {
			continue
		}
		// item.Memory.Content is already path-rewritten by the preview.
		// In repo-scoped mode we stamp every memory with the matched
		// git-origin key so the server groups them into one project by origin,
		// regardless of any stale per-memory key.
		rpk := derefString(item.Memory.RemoteProjectKey)
		if proj != nil && proj.RemoteKey != "" {
			rpk = pushKey
		}
		pushMemories = append(pushMemories, sync.SyncMemory{
			ID:               item.Memory.ID,
			Category:         item.Memory.Category,
			Content:          item.Memory.Content,
			Source:           item.Memory.Source,
			PreferenceScope:  item.Memory.PreferenceScope,
			RemoteProjectKey: rpk,
			Metadata:         item.Memory.Metadata,
		})
	}

	pushReq := sync.SyncPushRequest{
		ClientID:    "cli",
		ProjectID:   *projectID,
		Memories:    pushMemories,
		ProjectRoot: projectRoot,
	}
	// Send a friendly claim so a project auto-created by origin gets the repo's
	// name instead of "auto-<hash>". Only when routing by origin (no forced id).
	if *projectID == "" && proj != nil && proj.RemoteKey != "" {
		pushReq.ProjectClaim = &sync.ProjectClaim{Name: proj.Name, RemoteKey: pushKey}
		fmt.Printf("Repo %q · origin %s · key %s → remote project %q\n",
			proj.Name, orEmpty(gitOriginURL(projectRoot), "(none)"), pushKey, proj.Name)
	}

	resp, err := client.Push(ctx, pushReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Push complete: %d accepted, %d rejected\n", resp.Accepted, resp.Rejected)
	if len(resp.Errors) > 0 {
		for _, e := range resp.Errors {
			fmt.Fprintf(os.Stderr, "  error: %s\n", e)
		}
	}
	if line := policyHintLine(resp.Policy); line != "" {
		fmt.Println(line)
	}

	if proj != nil {
		kgInst := kg.New(svc.StoreDB(), logger)
		localTriples, kgErr := kgInst.ListByProject(ctx, proj.ID)
		if kgErr != nil {
			fmt.Fprintf(os.Stderr, "KG sync: warning: list triples failed: %v\n", kgErr)
		} else if len(localTriples) > 0 {
			syncTriples := make([]sync.SyncTriple, len(localTriples))
			for i, t := range localTriples {
				syncTriples[i] = sync.SyncTriple{
					Subject:    t.Subject,
					Predicate:  t.Predicate,
					Object:     t.Object,
					Confidence: t.Confidence,
				}
			}
			// Resolve the remote project for the triples endpoint, which
			// needs a concrete ID (no project_claim routing): prefer the
			// ID the server resolved for this push (servers >= v0.4.4),
			// then an explicit --project-id/link, then a remote_key match
			// against the server's project list (older servers).
			remoteProjID := resp.ProjectID
			if remoteProjID == "" {
				remoteProjID = *projectID
			}
			if remoteProjID == "" && proj.RemoteKey != "" {
				remoteProjID, _ = client.ResolveProjectIDByRemoteKeys(ctx, canonicalKey, legacyKey)
			}
			if remoteProjID == "" {
				fmt.Fprintln(os.Stderr, "KG sync: skipped: could not resolve the remote project ID (server did not return one and no remote_key match)")
				return
			}
			kgResp, kgPushErr := client.PushTriples(ctx, remoteProjID, syncTriples)
			if kgPushErr != nil {
				fmt.Fprintf(os.Stderr, "KG sync: warning: push triples failed: %v\n", kgPushErr)
			} else {
				fmt.Printf("KG sync: %d accepted, %d rejected\n", kgResp.Accepted, kgResp.Rejected)
				if len(kgResp.Errors) > 0 {
					for _, e := range kgResp.Errors {
						fmt.Fprintf(os.Stderr, "  KG error: %s\n", e)
					}
				}
			}
		} else {
			fmt.Println("KG sync: no local triples to push")
		}
	}
}

// remoteNames returns the configured remote names, comma-separated, for
// error messages.
func remoteNames(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Remotes)+1)
	for name := range cfg.Remotes {
		names = append(names, name)
	}
	// Commands that load the config raw (without the legacy-block migration)
	// won't have "default" in the map even though --remote default is valid
	// whenever the singular remote: block is configured — list it too.
	if cfg.Remote.ServerURL != "" {
		if _, ok := cfg.Remotes["default"]; !ok {
			names = append(names, "default")
		}
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// listProjectMemories paginates through every non-deleted memory belonging to a
// single local project.
func listProjectMemories(ctx context.Context, svc *memory.Service, projectID string) ([]memory.Memory, error) {
	const pageSize = 1000
	var all []memory.Memory
	offset := 0
	for {
		page, err := svc.List(ctx, memory.ListOptions{
			Limit:          pageSize,
			Offset:         offset,
			ProjectID:      projectID,
			IncludeDeleted: false,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
		offset += pageSize
	}
}


// listAllMemories paginates through every non-deleted memory.
// Using a fixed Limit caps the result set; without pagination, large stores
// silently drop rows past the cap.
func listAllMemories(ctx context.Context, svc *memory.Service) ([]memory.Memory, error) {
	const pageSize = 1000
	var all []memory.Memory
	offset := 0
	for {
		page, err := svc.List(ctx, memory.ListOptions{
			Limit:          pageSize,
			Offset:         offset,
			IncludeDeleted: false,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
		offset += pageSize
	}
}

func toSyncMemories(memories []memory.Memory) []sync.Memory {
	out := make([]sync.Memory, len(memories))
	for i, m := range memories {
		out[i] = sync.Memory{
			ID:               m.ID,
			Category:         m.Category,
			Content:          m.Content,
			ProjectID:        m.ProjectID,
			Source:           m.Source,
			SyncOrigin:       m.SyncOrigin,
			SyncDirty:        m.SyncDirty,
			RemoteProjectKey: m.RemoteProjectKey,
			PreferenceScope:  preferenceScopeFromMetadata(m.Metadata),
			Metadata:         m.Metadata,
		}
	}
	return out
}

func preferenceScopeFromMetadata(v any) string {
	switch m := v.(type) {
	case memory.MemoryMetadata:
		return m.PreferenceScope
	case map[string]any:
		s, _ := m["scope"].(string)
		return s
	default:
		return memory.ParseMetadata(v).PreferenceScope
	}
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// gitOriginURL returns the raw `git remote get-url origin` for a directory, or
// "" when there is no repo/origin. Used only for human-readable confirmation
// output; routing relies on the detector's derived remote_key, not this string.
func gitOriginURL(dir string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remoteKeysMatch reports whether any of the remote project's routing keys
// (canonical or legacy) equals any of the repo's derived keys. Empty keys
// never match.
func remoteKeysMatch(rp *sync.RemoteProject, repoKeys ...string) bool {
	for _, k := range repoKeys {
		if k == "" {
			continue
		}
		if k == rp.RemoteKey || k == rp.RemoteKeyV1 {
			return true
		}
	}
	return false
}
