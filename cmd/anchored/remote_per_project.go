package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	projectpkg "github.com/jholhewres/anchored/pkg/project"
	"github.com/jholhewres/anchored/pkg/sync"
)

// perProjectOptions are the flags `remote sync-per-project` and `remote sync
// --all` share.
type perProjectOptions struct {
	configPath string
	// remote, when set, sends every project to this named remote instead of
	// routing each one.
	remote      string
	minMemories int
	fallback    string
	dryRun      bool
}

func defaultPerProjectOptions() perProjectOptions {
	return perProjectOptions{minMemories: 1, fallback: "misc"}
}

// runRemoteSyncPerProject pushes the local store one local-project at a time,
// creating one remote project per local-project so that the original
// segmentation is preserved. Memories without a local project are pushed to
// a fallback "misc" project.
//
// Usage: anchored remote sync-per-project [--config path] [--remote name] [--min-memories N] [--dry-run]
func runRemoteSyncPerProject(args []string) {
	fs := newFlagSet("remote sync-per-project")
	opts := defaultPerProjectOptions()
	fs.StringVar(&opts.configPath, "config", "", "path to config file")
	fs.StringVar(&opts.remote, "remote", "", "send every project to this named remote (default: the remote that knows the repository, else its path route, else the default remote)")
	fs.IntVar(&opts.minMemories, "min-memories", opts.minMemories, "skip local projects with fewer syncable memories than this")
	fs.StringVar(&opts.fallback, "fallback-name", opts.fallback, "remote project name for memories without a local project")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the plan without making network calls")
	fs.Parse(args)
	syncPerProject(opts)
}

type localProject struct {
	name, path, remoteKey string
}

// perProjectBucket is one local project's syncable memories, checked against
// that project's own root, and where they go.
type perProjectBucket struct {
	key      string // local project id, or "" for memories without one
	name     string
	local    localProject
	keys     []string
	memories []sync.SyncMemory
	entry    *config.RemoteEntry
}

// planPerProject groups memories by local project and classifies each group
// against its own project root, never the process cwd. Memories without a
// project, or whose project has no known path, get no root, so every local
// path in them stays blocked. Buckets come largest first, then by name and id.
func planPerProject(memories []sync.Memory, locals map[string]localProject, fallback string) []*perProjectBucket {
	byProject := map[string][]sync.Memory{}
	for _, m := range memories {
		pid := ""
		if m.ProjectID != nil {
			pid = *m.ProjectID
		}
		byProject[pid] = append(byProject[pid], m)
	}

	buckets := make([]*perProjectBucket, 0, len(byProject))
	for pid, group := range byProject {
		b := &perProjectBucket{key: pid, name: fallback}
		if pid != "" {
			b.local = locals[pid]
			b.name = localProjectLabel(pid, b.local.name)
		}
		preview := sync.ClassifyForPreview(group, b.local.path)
		for _, item := range preview.Items {
			if item.Classification != sync.ClassificationSyncable {
				continue
			}
			b.memories = append(b.memories, sync.SyncMemory{
				ID:               item.Memory.ID,
				Category:         item.Memory.Category,
				Content:          item.Memory.Content,
				Source:           item.Memory.Source,
				PreferenceScope:  item.Memory.PreferenceScope,
				RemoteProjectKey: derefString(item.Memory.RemoteProjectKey),
				Metadata:         item.Memory.Metadata,
			})
		}
		if len(b.memories) > 0 {
			buckets = append(buckets, b)
		}
	}
	sort.Slice(buckets, func(i, j int) bool {
		if ni, nj := len(buckets[i].memories), len(buckets[j].memories); ni != nj {
			return ni > nj
		}
		if buckets[i].name != buckets[j].name {
			return buckets[i].name < buckets[j].name
		}
		return buckets[i].key < buckets[j].key
	})
	return buckets
}

type perProjectTarget struct {
	client   *sync.Client
	admin    *adminAPI
	projects []sync.RemoteProject
	listed   bool
}

// listing is the remote's project list, fetched once per run unless refresh.
func (t *perProjectTarget) listing(ctx context.Context, refresh bool) ([]sync.RemoteProject, error) {
	if !t.listed || refresh {
		projects, err := t.client.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		t.projects, t.listed = projects, true
	}
	return t.projects, nil
}

func syncPerProject(opts perProjectOptions) {
	cfg, logger, svc, err := initService(opts.configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	var forced *config.RemoteEntry
	if opts.remote != "" {
		forced = namedRemoteOrExit(cfg, opts.remote)
	} else if len(cfg.Remotes) == 0 {
		if cfg.Remote.ServerURL != "" && !cfg.Remote.Enabled {
			fmt.Fprintln(os.Stderr, "Remote sync is disabled. Enable it in the config, or add a `remotes:` entry.")
		} else {
			fmt.Fprintln(os.Stderr, "No remote configured. Run `anchored remote configure --server <url> --key <key>` or add a `remotes:` entry to the config.")
		}
		os.Exit(1)
	}

	ctx := context.Background()

	memories, err := listAllMemories(ctx, svc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "list error: %v\n", err)
		os.Exit(1)
	}

	// Each project's name, path and routing key decide where its memories go,
	// so a store that cannot list them stops the run.
	locals, err := loadLocalProjects(ctx, svc.StoreDB())
	if err != nil {
		fmt.Fprintf(os.Stderr, "list local projects: %v\n", err)
		svc.Close()
		os.Exit(1)
	}
	buckets := planPerProject(toSyncMemories(memories), locals, opts.fallback)

	// The plan routes by path, then the default remote. When the run asks
	// the remotes, a repository already registered on another one goes
	// there instead, as `remote sync` and MCP writes do; that includes one
	// no path or default routes.
	probe := func(b *perProjectBucket) bool {
		return forced == nil && len(cfg.Remotes) > 1 && len(b.keys) > 0
	}
	syncable := 0
	for _, b := range buckets {
		syncable += len(b.memories)
		if len(b.memories) >= opts.minMemories {
			b.keys = localProjectKeys(b.local)
		}
		switch {
		case forced != nil:
			b.entry = forced
		case b.local.path != "":
			b.entry = cfg.ResolveRemote(b.local.path)
		default:
			b.entry = cfg.DefaultRemote()
		}
	}

	fmt.Printf("Total syncable: %d across %d local projects\n", syncable, len(buckets))
	fmt.Printf("Threshold: skipping projects with <%d memories\n\n", opts.minMemories)
	fmt.Println("Plan:")
	skipped := 0
	pushable := 0
	for _, b := range buckets {
		marker := ""
		switch {
		case len(b.memories) < opts.minMemories:
			marker = " (skip)"
			skipped++
		case b.entry == nil && !probe(b):
			marker = " (skip: no remote routes here)"
			skipped++
		case b.entry == nil:
			marker = " → the remote that knows it, if any"
			pushable += len(b.memories)
		default:
			marker = " → " + b.entry.Name
			pushable += len(b.memories)
		}
		fmt.Printf("  %-40s %5d%s\n", b.name, len(b.memories), marker)
	}
	fmt.Printf("\nWill push %d memories across %d projects (skipping %d)\n\n",
		pushable, len(buckets)-skipped, skipped)
	if opts.dryRun {
		if forced == nil && len(cfg.Remotes) > 1 {
			fmt.Println("Dry run: routes are by path and default only; a run sends a repository already registered on another remote to that remote.")
		}
		return
	}

	targets := map[string]*perProjectTarget{}
	targetFor := func(entry *config.RemoteEntry) *perProjectTarget {
		t, ok := targets[entry.Name]
		if !ok {
			t = &perProjectTarget{
				client: sync.NewClientFromEntry(*entry, "cli-per-project"),
				admin:  newAdminAPI(entry.ServerURL, entry.APIKey),
			}
			targets[entry.Name] = t
		}
		return t
	}
	kgInst := kg.New(svc.StoreDB(), logger)

	totalAccepted := 0
	totalRejected := 0
	unsynced := 0
	for _, b := range buckets {
		if len(b.memories) < opts.minMemories {
			continue
		}
		keys := b.keys
		entry := b.entry
		var projID, pushKey string
		if probe(b) {
			r := sync.ResolveProjectAcrossRemotesForWrite(ctx, cfg, b.local.path, "cli-per-project", keys...)
			if r.Held() {
				fmt.Fprintf(os.Stderr, "  [%s] held: %s; run the sync again once it answers, or pick a remote with --remote\n", b.name, r.HeldReason())
				unsynced++
				continue
			}
			if r.Entry != nil && r.ProjectID != "" {
				if entry == nil || r.Entry.Name != entry.Name {
					fmt.Printf("  [%s] registered on remote %q — using it\n", b.name, r.Entry.Name)
				}
				entry, projID, pushKey = r.Entry, r.ProjectID, r.MatchedKey
			}
		}
		if entry == nil {
			if probe(b) {
				fmt.Fprintf(os.Stderr, "  [%s] skipped: no remote knows it, and no path or default remote routes it\n", b.name)
			}
			continue
		}
		target := targetFor(entry)
		if projID == "" {
			projID, pushKey, err = remoteProjectFor(ctx, target, b.name, toRemoteSlug(b.name), keys)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  [%s] create project failed: %v\n", b.name, err)
				unsynced++
				continue
			}
		}
		if pushKey != "" {
			// The server groups a repository's memories by its git-origin
			// key, as repo-scoped sync stamps them.
			for i := range b.memories {
				b.memories[i].RemoteProjectKey = pushKey
			}
		}

		resp, err := target.client.Push(ctx, sync.SyncPushRequest{
			ClientID:    "cli-per-project",
			ProjectID:   projID,
			Memories:    b.memories,
			ProjectRoot: b.local.path,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] push failed: %v\n", b.name, err)
			unsynced++
			continue
		}
		fmt.Printf("  [%s] %d accepted, %d rejected (%s)\n", b.name, resp.Accepted, resp.Rejected, entry.Name)
		totalAccepted += resp.Accepted
		totalRejected += resp.Rejected

		if b.key == "" {
			continue
		}
		localTriples, kgErr := kgInst.ListByProject(ctx, b.key)
		if kgErr != nil {
			fmt.Fprintf(os.Stderr, "  [%s] KG sync: warning: list triples failed: %v\n", b.name, kgErr)
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
			kgResp, kgPushErr := target.client.PushTriples(ctx, projID, syncTriples)
			if kgPushErr != nil {
				fmt.Fprintf(os.Stderr, "  [%s] KG sync: warning: push triples failed: %v\n", b.name, kgPushErr)
			} else {
				fmt.Printf("  [%s] KG sync: %d accepted, %d rejected\n", b.name, kgResp.Accepted, kgResp.Rejected)
			}
		}
	}

	fmt.Printf("\nDone. %d accepted, %d rejected total.\n", totalAccepted, totalRejected)
	if unsynced > 0 {
		fmt.Fprintf(os.Stderr, "%d projects were held or failed; run the sync again.\n", unsynced)
		svc.Close()
		os.Exit(1)
	}
}

// localProjectLabel names a local project on the remote: its own name, or one
// made from its id when the local store has none for it.
func localProjectLabel(pid, name string) string {
	if name != "" {
		return name
	}
	return "project-" + shortID(pid)
}

// localProjectKeys lists the git-origin keys a local project is known by on a
// remote: the canonical key its checkout derives now, then the stored one (for
// a checkout that is gone or not re-detected yet), then the v1 key, without
// repeats. A project with no origin has none.
func localProjectKeys(p localProject) []string {
	var canonical, v1 string
	if p.path != "" {
		canonical, v1 = projectpkg.RemoteKeysFromDir(p.path)
	}
	var keys []string
	for _, k := range []string{canonical, p.remoteKey, v1} {
		if k != "" && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// localProjectKey is the routing key a project without a git origin is
// created under: stable, so later runs find the same project.
func localProjectKey(slug string) string { return "local:" + slug }

// remoteProjectFor finds the remote project a local project syncs to, or
// creates it, and returns its id and the git-origin key the remote knows it
// by (empty for a project without an origin). A repository is matched by its
// keys. A slug match is adopted only when the remote project has no
// repository of its own (see adoptableBySlug), so two repositories with the
// same folder name never share a project: the second is created under its
// slug with a suffix from its key.
func remoteProjectFor(ctx context.Context, target *perProjectTarget, name, slug string, keys []string) (projectID, pushKey string, err error) {
	createKey := localProjectKey(slug)
	if len(keys) > 0 {
		createKey = keys[0]
	}
	// find returns the project the local one maps to and the key to stamp its
	// memories with: the key the remote matched, else the create key.
	find := func(refresh bool) (id, key string, taken bool, err error) {
		projects, err := target.listing(ctx, refresh)
		if err != nil {
			return "", "", false, err
		}
		if id, matched := sync.MatchProjectByRemoteKeys(projects, keys...); id != "" {
			return id, matched, false, nil
		}
		for _, p := range projects {
			if p.Slug == slug {
				if adoptableBySlug(p, slug) {
					return p.ID, firstKey(keys), false, nil
				}
				return "", "", true, nil
			}
		}
		return "", "", false, nil
	}

	found, key, taken, err := find(false)
	if err != nil {
		return "", "", err
	}
	if found != "" {
		return found, key, nil
	}
	createSlug := slug
	if taken {
		if len(keys) == 0 {
			return "", "", fmt.Errorf("slug %q belongs to another repository's project on this remote; rename the local project or pick a remote with --remote", slug)
		}
		createSlug = slug + "-" + shortID(createKey)
	}
	id, cerr := target.admin.createProject(ctx, name, createSlug, createKey)
	if cerr == nil {
		target.projects = append(target.projects, sync.RemoteProject{ID: id, Name: name, Slug: createSlug, RemoteKey: createKey})
		return id, firstKey(keys), nil
	}
	// The server may have created it on an earlier attempt.
	if found, key, _, err := find(true); err == nil && found != "" {
		return found, key, nil
	}
	return "", "", cerr
}

func firstKey(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// adoptableBySlug reports whether a remote project found by slug alone may
// take a local project's memories: only one no repository owns, keyed empty,
// with the local key, or with the "<slug>-<unix time>" key earlier runs of
// this command gave every project.
func adoptableBySlug(p sync.RemoteProject, slug string) bool {
	if p.RemoteKey == "" || p.RemoteKey == localProjectKey(slug) {
		return true
	}
	rest, ok := strings.CutPrefix(p.RemoteKey, slug+"-")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func loadLocalProjects(ctx context.Context, db *sql.DB) (map[string]localProject, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name, path, COALESCE(remote_key, '') FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]localProject{}
	for rows.Next() {
		var id string
		var p localProject
		if err := rows.Scan(&id, &p.name, &p.path, &p.remoteKey); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

var slugInvalidRe = regexp.MustCompile(`[^a-z0-9-]+`)

func toRemoteSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = slugInvalidRe.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "project"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

type adminAPI struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func newAdminAPI(baseURL, apiKey string) *adminAPI {
	return &adminAPI{baseURL: baseURL, apiKey: apiKey, client: &http.Client{Timeout: 60 * time.Second}}
}

func (a *adminAPI) createProject(ctx context.Context, name, slug, remoteKey string) (string, error) {
	buf, err := json.Marshal(map[string]string{"name": name, "slug": slug, "remote_key": remoteKey})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/projects", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read create project response: %w", err)
	}
	if resp.StatusCode >= 400 {
		// The body is the server's text: quoted, so it cannot drive the terminal.
		return "", fmt.Errorf("create project %d: %q", resp.StatusCode, truncateUTF8(string(respBody), 200))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("create project: the server returned no id")
	}
	return out.ID, nil
}
