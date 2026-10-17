package config

import (
	"os"
	"strings"
	"testing"
)

// TestMigrateRemotes_MergesSingularIntoMap locks the multi-server contract:
// the legacy singular `remote:` block must keep resolving as "default" even
// when named `remotes:` entries exist — otherwise adding a company server
// would silently drop the personal one from routing.
func TestMigrateRemotes_MergesSingularIntoMap(t *testing.T) {
	cfg := Defaults()
	cfg.Remote.Enabled = true
	cfg.Remote.ServerURL = "https://personal.example"
	cfg.Remote.APIKey = "k1"
	cfg.Remote.Projects = []string{"proj-1"}
	cfg.Remotes = map[string]RemoteEntry{
		"company": {ServerURL: "https://company.example", APIKey: "k2", Paths: []string{"/home/me/work/*"}},
	}

	cfg.migrateRemotes()

	def, ok := cfg.Remotes["default"]
	if !ok {
		t.Fatal("singular remote was not merged into the map as \"default\"")
	}
	if !def.Default {
		t.Error("merged default entry must carry Default=true when no other entry has it")
	}
	if def.ServerURL != "https://personal.example" || len(def.Projects) != 1 {
		t.Errorf("merged default lost fields: %+v", def)
	}
	if cfg.Remotes["company"].Name != "company" {
		t.Errorf("named entry Name not normalized: %+v", cfg.Remotes["company"])
	}
}

// TestResolveRemote_PathsRouteAndDefaultFallback verifies routing: paths
// globs pick the matching remote, everything else falls back to the default.
// Projects (linked IDs) must NOT participate in routing.
func TestResolveRemote_PathsRouteAndDefaultFallback(t *testing.T) {
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"default": {Name: "default", ServerURL: "https://personal.example", Default: true},
		"company": {
			Name:      "company",
			ServerURL: "https://company.example",
			Paths:     []string{"/home/me/work/*"},
			Projects:  []string{"/home/me/oss/repo"}, // a linked ID that looks like a path — must not route
		},
	}

	if e := cfg.ResolveRemote("/home/me/work/api"); e == nil || e.Name != "company" {
		t.Errorf("work path should route to company, got %+v", e)
	}
	if e := cfg.ResolveRemote("/home/me/oss/repo"); e == nil || e.Name != "default" {
		t.Errorf("projects (linked IDs) must not route; expected default, got %+v", e)
	}
	if e := cfg.ResolveRemote("/anywhere/else"); e == nil || e.Name != "default" {
		t.Errorf("unmatched path should fall back to default, got %+v", e)
	}
}

// A singular block turned off (`remote configure --disable`) stays out of the
// map: `remote status` reports it disabled, so it must not keep routing
// searches and syncs to its server.
func TestMigrateRemotes_SkipsADisabledSingularBlock(t *testing.T) {
	cfg := Defaults()
	cfg.Remote.Enabled = false
	cfg.Remote.ServerURL = "https://gone.example"
	cfg.Remote.APIKey = "k1"
	cfg.Remotes = map[string]RemoteEntry{
		"company": {ServerURL: "https://company.example", APIKey: "k2"},
	}

	cfg.migrateRemotes()

	if _, ok := cfg.Remotes["default"]; ok {
		t.Fatal("a disabled singular remote was merged into routing")
	}
	if cfg.ResolveRemote("/anywhere") != nil {
		t.Fatal("a disabled default remote still resolves")
	}
	if _, ok := cfg.Remotes["company"]; !ok {
		t.Fatal("named remotes must be unaffected")
	}
}

// A tie between path globs goes to the most specific pattern, the one with the
// most literal characters, and then to the remote's name: the same repository must reach the same
// server on every run, not whichever map entry Go happens to visit first.
func TestResolveRemote_GlobTieIsDeterministic(t *testing.T) {
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"broad":   {Name: "broad", ServerURL: "https://broad.example", Paths: []string{"/home/me/*/*"}},
		"work":    {Name: "work", ServerURL: "https://work.example", Paths: []string{"/home/me/work/*"}},
		"work-b":  {Name: "work-b", ServerURL: "https://work-b.example", Paths: []string{"/home/me/work/?pi"}},
		"other-a": {Name: "other-a", ServerURL: "https://a.example", Paths: []string{"/srv/*/repo"}},
		"other-b": {Name: "other-b", ServerURL: "https://b.example", Paths: []string{"/srv/x*/repo"}},
	}

	for i := 0; i < 100; i++ {
		if e := cfg.ResolveRemote("/home/me/work/api"); e == nil || e.Name != "work-b" {
			t.Fatalf("run %d: /home/me/work/api routed to %+v, want work-b (most literal characters)", i, e)
		}
		if e := cfg.ResolveRemote("/home/me/oss/lib"); e == nil || e.Name != "broad" {
			t.Fatalf("run %d: /home/me/oss/lib routed to %+v, want broad", i, e)
		}
		if e := cfg.ResolveRemote("/srv/xy/repo"); e == nil || e.Name != "other-b" {
			t.Fatalf("run %d: /srv/xy/repo routed to %+v, want other-b (most literal characters)", i, e)
		}
	}

	cfg.Remotes = map[string]RemoteEntry{
		"zeta":  {Name: "zeta", ServerURL: "https://z.example", Paths: []string{"/home/me/*"}},
		"alpha": {Name: "alpha", ServerURL: "https://a.example", Paths: []string{"/home/*e/x"}},
	}
	for i := 0; i < 100; i++ {
		if e := cfg.ResolveRemote("/home/me/x"); e == nil || e.Name != "alpha" {
			t.Fatalf("run %d: equal-length tie routed to %+v, want alpha (first by name)", i, e)
		}
	}
}

// Several remotes marked default resolve to the first by name, and
// DefaultRemoteNames lists them all so `remote status` can say so.
func TestResolveRemote_DuplicateDefaultsAreDeterministic(t *testing.T) {
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
		"company":  {Name: "company", ServerURL: "https://c.example", Default: true},
		"lab":      {Name: "lab", ServerURL: "https://l.example"},
	}

	for i := 0; i < 100; i++ {
		if e := cfg.ResolveRemote("/anywhere"); e == nil || e.Name != "company" {
			t.Fatalf("run %d: duplicate defaults resolved to %+v, want company (first by name)", i, e)
		}
	}
	got := cfg.DefaultRemoteNames()
	if len(got) != 2 || got[0] != "company" || got[1] != "personal" {
		t.Errorf("DefaultRemoteNames() = %v, want [company personal]", got)
	}
}

// Specificity counts literal characters: a broad pattern padded with a class
// or wildcards does not outrank a narrower one that names the directory.
func TestResolveRemote_MetacharactersDoNotMakeAGlobSpecific(t *testing.T) {
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"personal": {Name: "personal", ServerURL: "https://p.example", Paths: []string{"/home/me/[a-z]*/*"}},
		"company":  {Name: "company", ServerURL: "https://c.example", Paths: []string{"/home/me/acme/*"}},
	}
	entry, pattern := cfg.ResolveRemoteWithPattern("/home/me/acme/api")
	if entry == nil || entry.Name != "company" || pattern != "/home/me/acme/*" {
		t.Fatalf("/home/me/acme/api routed to %+v by %q, want company by /home/me/acme/*", entry, pattern)
	}
	if entry, pattern := cfg.ResolveRemoteWithPattern("/home/me/oss/lib"); entry == nil || entry.Name != "personal" || pattern != "/home/me/[a-z]*/*" {
		t.Fatalf("/home/me/oss/lib routed to %+v by %q, want personal", entry, pattern)
	}
}

// A glob routes the directory it matches and everything below it, so a
// command run from a repository's subdirectory, or with a relative path,
// routes like the repository itself.
func TestResolveRemote_GlobCoversSubdirectoriesAndRelativePaths(t *testing.T) {
	base := t.TempDir()
	repo := base + "/work/api"
	if err := os.MkdirAll(repo+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{base + "/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	for _, p := range []string{repo, repo + "/pkg/sync", repo + "/"} {
		if e, pattern := cfg.ResolveRemoteWithPattern(p); e == nil || e.Name != "work" || pattern != base+"/work/*" {
			t.Errorf("%s routed to %+v by %q, want work", p, e, pattern)
		}
	}
	if e := cfg.ResolveRemote(base + "/home/notes"); e == nil || e.Name != "personal" {
		t.Errorf("a path no glob covers routed to %+v, want personal", e)
	}

	if err := os.MkdirAll(repo+"/pkg", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo + "/pkg")
	for _, p := range []string{".", "../pkg"} {
		if e := cfg.ResolveRemote(p); e == nil || e.Name != "work" {
			t.Errorf("relative %q routed to %+v, want work", p, e)
		}
	}
	// No path at all routes to the default remote, as before.
	if e := cfg.ResolveRemote(""); e == nil || e.Name != "personal" {
		t.Errorf("an empty path routed to %+v, want personal", e)
	}
}

// A glob also covers the same repository reached another way: through a
// symlink, or as a linked worktree elsewhere whose main checkout it matches.
func TestResolveRemote_GlobCoversSymlinksAndLinkedWorktrees(t *testing.T) {
	base := t.TempDir()
	repo := base + "/work/api"
	if err := os.MkdirAll(repo+"/.git/worktrees/wt", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base+"/work", base+"/w"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	wt := base + "/scratch/wt"
	if err := os.MkdirAll(wt+"/pkg", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wt+"/.git", []byte("gitdir: "+repo+"/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{base + "/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	for _, p := range []string{base + "/w/api", wt, wt + "/pkg"} {
		if e, _ := cfg.ResolveRemoteWithPattern(p); e == nil || e.Name != "work" {
			t.Errorf("%s routed to %+v, want work", p, e)
		}
	}
	if e := cfg.ResolveRemote(base + "/scratch"); e == nil || e.Name != "personal" {
		t.Errorf("a directory outside the worktree routed to %+v, want personal", e)
	}
}

// A glob covers what it matches and the repositories inside it, never the
// directories above a repository's root: "/home/me/*" does not take over a
// repository at /home/me/work/x.
func TestResolveRemote_GlobDoesNotReachAboveARepositoryRoot(t *testing.T) {
	base := t.TempDir()
	nested := base + "/work/x"
	if err := os.MkdirAll(nested+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"personal": {Name: "personal", ServerURL: "https://p.example", Paths: []string{base + "/*"}},
		"company":  {Name: "company", ServerURL: "https://c.example", Default: true},
	}
	for _, p := range []string{nested, nested + "/pkg"} {
		if e, pattern := cfg.ResolveRemoteWithPattern(p); e == nil || e.Name != "company" || pattern != "" {
			t.Errorf("%s routed to %+v by %q, want the default company", p, e, pattern)
		}
	}
}

// Only a small regular .git file is read for a worktree's main checkout.
func TestResolveRemote_IgnoresAnOversizedGitFile(t *testing.T) {
	base := t.TempDir()
	wt := base + "/scratch/wt"
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	big := "gitdir: " + base + "/work/api/.git/worktrees/wt\n" + strings.Repeat("#", 8<<10)
	if err := os.WriteFile(wt+"/.git", []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{base + "/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	if e := cfg.ResolveRemote(wt); e == nil || e.Name != "personal" {
		t.Errorf("an oversized .git steered %s to %+v, want personal", wt, e)
	}
}

// A .git that is not a regular file, a symlink included, is not followed to a
// gitdir line.
func TestResolveRemote_IgnoresASymlinkedGitFile(t *testing.T) {
	base := t.TempDir()
	wt := base + "/scratch/wt"
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	target := base + "/gitfile"
	if err := os.WriteFile(target, []byte("gitdir: "+base+"/work/api/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, wt+"/.git"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{base + "/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	if e := cfg.ResolveRemote(wt); e == nil || e.Name != "personal" {
		t.Errorf("a symlinked .git steered %s to %+v, want personal", wt, e)
	}
}

// A glob written through a symlinked directory (macOS's /var is
// /private/var) matches the repository under its resolved path too.
func TestResolveRemote_GlobWrittenThroughASymlink(t *testing.T) {
	real := t.TempDir()
	link := t.TempDir() + "/link"
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.MkdirAll(real+"/work/api/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{link + "/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	for _, p := range []string{real + "/work/api", link + "/work/api", real + "/work/api/pkg"} {
		if e, pattern := cfg.ResolveRemoteWithPattern(p); e == nil || e.Name != "work" || pattern != link+"/work/*" {
			t.Errorf("%s routed to %+v by %q, want work by the glob as written", p, e, pattern)
		}
	}

	// The macOS shape on any OS: the glob names the resolved directory and the
	// command runs from a not-yet-created subdirectory reached through a link,
	// so only the repository root, resolved, can match.
	cfg.Remotes["work"] = RemoteEntry{Name: "work", ServerURL: "https://w.example", Paths: []string{real + "/work/*"}}
	if e := cfg.ResolveRemote(link + "/work/api/new"); e == nil || e.Name != "work" {
		t.Errorf("an uncreated subdirectory reached through a link routed to %+v, want work", e)
	}
}

// A resolved directory is spliced into the glob as literal text, so glob
// characters in its name match only themselves; and a relative glob, which
// can never match an absolute repository path, is not resolved against the
// working directory.
func TestResolveRemote_ResolvedGlobDirectoryIsLiteralAndRelativeGlobsStayUnresolved(t *testing.T) {
	base := t.TempDir()
	real := base + "/re[a]l"
	if err := os.MkdirAll(real+"/work/api/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, base+"/link"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{base + "/link/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	if e := cfg.ResolveRemote(real + "/work/api"); e == nil || e.Name != "work" {
		t.Errorf("a resolved directory with glob characters routed to %+v, want work", e)
	}

	plain := base + "/plain"
	if err := os.MkdirAll(plain+"/work/api/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plain, base+"/plainlink"); err != nil {
		t.Fatal(err)
	}
	cfg.Remotes = map[string]RemoteEntry{
		"work":     {Name: "work", ServerURL: "https://w.example", Paths: []string{"plainlink/work/*"}},
		"personal": {Name: "personal", ServerURL: "https://p.example", Default: true},
	}
	t.Chdir(base)
	if e := cfg.ResolveRemote(plain + "/work/api"); e == nil || e.Name != "personal" {
		t.Errorf("a relative glob resolved against the working directory routed to %+v, want personal", e)
	}
}
