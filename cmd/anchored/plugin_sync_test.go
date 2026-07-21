package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.4.5", "0.4.6", -1},
		{"0.4.6", "0.4.6", 0},
		{"0.4.7", "0.4.6", 1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.4.6-rc1", "0.4.6", 0}, // prerelease ignored
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLooksLikeSemver(t *testing.T) {
	yes := []string{"0.4.6", "v1.2.3", "10.20.30"}
	no := []string{"latest", "0.4", "v0", "0..1.2", "abc", ""}
	for _, v := range yes {
		if !looksLikeSemver(strings.TrimPrefix(v, "v")) {
			t.Errorf("expected semver: %q", v)
		}
	}
	for _, v := range no {
		if looksLikeSemver(strings.TrimPrefix(v, "v")) {
			t.Errorf("expected NOT semver: %q", v)
		}
	}
}

// TestNewestInstalledVersion seeds a fake cache dir with multiple versions
// (including garbage) and confirms the highest semver wins.
func TestNewestInstalledVersion(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"0.3.9", "0.4.0", "0.4.6", "ignored", "0.4.2"} {
		pluginJSON := filepath.Join(dir, v, ".claude-plugin", "plugin.json")
		if err := os.MkdirAll(filepath.Dir(pluginJSON), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pluginJSON, []byte(`{"version":"`+v+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := newestInstalledVersion(dir); got != "0.4.6" {
		t.Fatalf("newest = %q, want 0.4.6", got)
	}
}

func TestNewestInstalledVersion_MissingDirReturnsEmpty(t *testing.T) {
	if got := newestInstalledVersion(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("got %q, want empty for missing dir", got)
	}
}

// TestNewestInstalledVersion_IgnoresVersionDirsWithoutManifest guards against
// counting half-installed or unrelated directories sharing the cache root.
func TestNewestInstalledVersion_IgnoresVersionDirsWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "9.9.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := newestInstalledVersion(dir); got != "" {
		t.Errorf("got %q, want empty when no plugin.json exists", got)
	}
}

func TestDetectPluginDrift(t *testing.T) {
	cacheDir := t.TempDir()
	mirrorDir := t.TempDir()
	seedPluginCache(t, cacheDir, "0.4.0")
	seedMirrorManifest(t, mirrorDir, "0.4.0")

	cfg := &config.Config{}
	cfg.Plugin.CacheDir = cacheDir
	cfg.Plugin.MarketplaceDir = mirrorDir

	// Mirror trails binary 0.4.6 (pull trigger) but the cache matches the mirror,
	// so there is nothing to install: MirrorBehind set, CacheBehind not. The
	// plugin is versioned independently, so a mirror behind the binary is not
	// user-facing drift on its own.
	d := detectPluginDrift(cfg, "0.4.6")
	if !d.HasDrift || !d.MirrorBehind || d.CacheBehind {
		t.Fatalf("expected mirror-behind only (cache matches mirror), got %+v", d)
	}
	if d.CacheVersion != "0.4.0" || d.MirrorVersion != "0.4.0" || d.BinaryVersion != "0.4.6" {
		t.Errorf("version fields wrong: %+v", d)
	}

	// Cache older than the mirror: real, user-facing drift.
	seedPluginCache(t, cacheDir, "0.4.0")
	freshMirrorDir := t.TempDir()
	seedMirrorManifest(t, freshMirrorDir, "0.5.0")
	cfgStale := &config.Config{}
	cfgStale.Plugin.CacheDir = cacheDir
	cfgStale.Plugin.MarketplaceDir = freshMirrorDir
	if ds := detectPluginDrift(cfgStale, "0.5.0"); !ds.CacheBehind {
		t.Fatalf("cache 0.4.0 < mirror 0.5.0 must be CacheBehind, got %+v", ds)
	}

	// All matching versions: no drift.
	d2 := detectPluginDrift(cfg, "0.4.0")
	if d2.HasDrift {
		t.Fatalf("expected no drift, got %+v", d2)
	}

	// Dev binary: never drifts (placeholder version is meaningless).
	d3 := detectPluginDrift(cfg, "dev")
	if d3.HasDrift {
		t.Fatalf("dev binary must never be considered drifting, got %+v", d3)
	}

	// Cache absent but mirror current: CacheBehind only — anchored cannot
	// fix this, user must run /plugin install. MirrorBehind must NOT be set.
	freshMirror := t.TempDir()
	seedMirrorManifest(t, freshMirror, "0.4.6")
	cfg2 := &config.Config{}
	cfg2.Plugin.CacheDir = t.TempDir() // empty
	cfg2.Plugin.MarketplaceDir = freshMirror
	d4 := detectPluginDrift(cfg2, "0.4.6")
	if !d4.CacheBehind || d4.MirrorBehind {
		t.Fatalf("expected CacheBehind only, got %+v", d4)
	}
}

// TestApplyPluginAutoUpdate_FullSync proves the v0.4.10 contract: when both
// mirror and cache are behind, the auto-update path (1) fast-forwards the
// git mirror, (2) copies the new tree into the cache, and (3) updates
// Claude Code's installed_plugins.json — all without user intervention.
// Every path is isolated: no real ~/.claude is touched.
func TestApplyPluginAutoUpdate_FullSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available in PATH; skipping fast-forward test")
	}
	upstream := t.TempDir()
	runGit(t, upstream, "init", "-q", "-b", "main")
	runGit(t, upstream, "config", "user.email", "test@test")
	runGit(t, upstream, "config", "user.name", "test")
	seedMirrorManifest(t, upstream, "0.4.0")
	runGit(t, upstream, "add", ".")
	runGit(t, upstream, "commit", "-q", "-m", "v0.4.0")

	mirror := t.TempDir()
	runGit(t, "", "clone", "-q", upstream, mirror)

	// Upstream bumps to 0.4.6.
	seedMirrorManifest(t, upstream, "0.4.6")
	runGit(t, upstream, "commit", "-aq", "-m", "v0.4.6")

	cacheDir := t.TempDir()
	seedPluginCache(t, cacheDir, "0.4.0")

	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	writeJSON(t, registryPath, map[string]any{"version": 2, "plugins": map[string]any{}})

	d := PluginDrift{
		BinaryVersion:  "0.4.6",
		MirrorVersion:  "0.4.0",
		CacheVersion:   "0.4.0",
		HasDrift:       true,
		MirrorBehind:   true,
		CacheBehind:    true,
		MarketplaceDir: mirror,
		CacheDir:       cacheDir,
		RegistryPath:   registryPath, // isolate from $HOME/.claude
	}
	out := applyPluginAutoUpdate(d)
	if !out.SyncPerformed || !out.CacheInstalled {
		t.Fatalf("expected both sync_performed and cache_installed, got %+v", out)
	}
	if out.SyncError != "" || out.CacheInstallError != "" {
		t.Fatalf("expected clean run, got %+v", out)
	}

	// Mirror plugin.json reflects new version.
	if v, _ := pluginManifestVersion(filepath.Join(mirror, ".claude-plugin", "plugin.json")); v != "0.4.6" {
		t.Errorf("mirror plugin.json version = %q, want 0.4.6", v)
	}
	// New cache dir exists with the mirror's plugin.json copied in.
	if v, _ := pluginManifestVersion(filepath.Join(cacheDir, "0.4.6", ".claude-plugin", "plugin.json")); v != "0.4.6" {
		t.Errorf("cache plugin.json missing or stale: got %q", v)
	}
	// Old cache dir is preserved (we don't sweep prior versions).
	if _, err := os.Stat(filepath.Join(cacheDir, "0.4.0")); err != nil {
		t.Errorf("prior cache version should be preserved, stat err=%v", err)
	}
	// Registry points at the new install path.
	var doc map[string]any
	readJSON(t, registryPath, &doc)
	entry := doc["plugins"].(map[string]any)["anchored@anchored"].([]any)[0].(map[string]any)
	if entry["version"] != "0.4.6" {
		t.Errorf("registry version = %v, want 0.4.6", entry["version"])
	}
	if entry["installPath"] != filepath.Join(cacheDir, "0.4.6") {
		t.Errorf("registry installPath = %v, want %s", entry["installPath"], filepath.Join(cacheDir, "0.4.6"))
	}
}

// TestApplyPluginAutoUpdate_CacheBehindOnly covers the partial-drift case:
// the mirror is current but the cache lags (e.g., user ran an earlier
// anchored that wiped the cache without reinstalling). Auto-update must
// still install from the mirror without trying to git pull.
func TestApplyPluginAutoUpdate_CacheBehindOnly(t *testing.T) {
	mirror := t.TempDir()
	cacheDir := t.TempDir()
	registryPath := filepath.Join(t.TempDir(), "installed_plugins.json")
	seedMirrorManifest(t, mirror, "0.4.6")
	writeJSON(t, registryPath, map[string]any{"version": 2, "plugins": map[string]any{}})

	out := applyPluginAutoUpdate(PluginDrift{
		HasDrift:       true,
		CacheBehind:    true,
		MirrorBehind:   false,
		BinaryVersion:  "0.4.6",
		MirrorVersion:  "0.4.6",
		MarketplaceDir: mirror,
		CacheDir:       cacheDir,
		RegistryPath:   registryPath,
	})
	if out.SyncPerformed {
		t.Error("CacheBehind-only must NOT run git pull (mirror is current)")
	}
	if !out.CacheInstalled {
		t.Errorf("CacheBehind-only must install the plugin, got %+v", out)
	}
	if out.CacheInstallError != "" {
		t.Errorf("unexpected install error: %q", out.CacheInstallError)
	}
}

func TestApplyPluginAutoUpdate_NoDriftIsNoOp(t *testing.T) {
	out := applyPluginAutoUpdate(PluginDrift{HasDrift: false})
	if out.SyncPerformed {
		t.Error("should not perform sync when HasDrift=false")
	}
}

func TestRenderPluginUpdateNotice(t *testing.T) {
	// MirrorBehind + AutoUpdate off: user has to drive both steps.
	manual := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, MirrorBehind: true, CacheBehind: true,
		BinaryVersion: "0.4.6", MirrorVersion: "0.4.0", CacheVersion: "0.4.0",
	})
	for _, want := range []string{
		`<anchored_plugin_update binary="0.4.6" mirror="0.4.0" cache="0.4.0">`,
		"/plugin marketplace update anchored",
		"/plugin install anchored@anchored",
		"</anchored_plugin_update>",
	} {
		if !strings.Contains(manual, want) {
			t.Errorf("manual notice missing %q\n--- output ---\n%s", want, manual)
		}
	}

	// MirrorBehind + auto-synced: anchored did the pull, user runs /plugin install.
	synced := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, MirrorBehind: true, CacheBehind: true, SyncPerformed: true,
		BinaryVersion: "0.4.6", MirrorVersion: "0.4.6", CacheVersion: "0.4.0",
	})
	if !strings.Contains(synced, `mirror_synced="true"`) || !strings.Contains(synced, "auto-synced to v0.4.6") {
		t.Errorf("expected mirror_synced markup, got %q", synced)
	}
	if !strings.Contains(synced, "/plugin install anchored@anchored") {
		t.Errorf("synced notice must still prompt /plugin install, got %q", synced)
	}

	// Sync-failed: embed the truncated error, fall back to manual instructions.
	failed := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, MirrorBehind: true,
		BinaryVersion: "0.4.6", MirrorVersion: "0.4.0",
		SyncError: "git pull failed: divergent history",
	})
	if !strings.Contains(failed, "git pull failed: divergent history") {
		t.Errorf("expected sync error in notice, got %q", failed)
	}

	// CacheBehind only: short notice telling user to run /plugin install.
	cacheOnly := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, CacheBehind: true, MirrorBehind: false,
		BinaryVersion: "0.4.6", MirrorVersion: "0.4.6",
	})
	if !strings.Contains(cacheOnly, `cache="absent"`) && !strings.Contains(cacheOnly, `cache=`) {
		t.Errorf("cache-only notice should expose cache attribute, got %q", cacheOnly)
	}
	if !strings.Contains(cacheOnly, "/plugin install anchored@anchored") {
		t.Errorf("cache-only notice missing /plugin install, got %q", cacheOnly)
	}

	// No drift = empty string.
	if got := renderPluginUpdateNotice(PluginDrift{HasDrift: false}); got != "" {
		t.Errorf("no-drift notice should be empty, got %q", got)
	}

	// Steady state: mirror trails the binary but the cache matches the mirror,
	// so nothing was installed and nothing failed. The notice must stay silent
	// instead of nagging "auto-updated ..." on every session.
	steady := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, MirrorBehind: true, CacheBehind: false, SyncPerformed: true,
		BinaryVersion: "0.15.0-beta.1", MirrorVersion: "0.12.0", CacheVersion: "0.12.0",
	})
	if steady != "" {
		t.Errorf("steady-state (cache==mirror, mirror<binary) must be silent, got %q", steady)
	}
}

// TestLooksLikeSemver_RejectsGarbage covers the strings that the tighter
// implementation must reject (CR-1 in the v0.4.7 code review).
func TestLooksLikeSemver_RejectsGarbage(t *testing.T) {
	bad := []string{
		"0.4.6.extra",  // 4 numeric segments
		"0.4.6foo",     // trailing letters
		"abc.def.ghi",  // non-numeric
		"1.2",          // < 3 segments
		"1..2.3",       // empty middle
		"1.2.3.4.5",    // > 3 segments
		"-1.2.3",       // strconv.Atoi accepts "-1" but the version namespace doesn't
	}
	for _, v := range bad {
		if looksLikeSemver(v) {
			t.Errorf("looksLikeSemver(%q) should be false", v)
		}
	}
}

// TestSemverTriple_HandlesOverflow guards against the previous naïve
// int-accumulation that would have wrapped on huge numbers.
func TestSemverTriple_HandlesOverflow(t *testing.T) {
	// "99999999999999999999" overflows int on 64-bit too; Atoi returns an
	// error and we fall back to digit-trim → returns 0 cleanly.
	got := semverTriple("99999999999999999999.0.0")
	if got[0] < 0 {
		t.Errorf("expected non-negative on overflow, got %v", got)
	}
}

// TestRenderPluginUpdateNotice_TruncatesLongSyncError protects against a
// verbose git stderr blowing up the bundle size.
func TestRenderPluginUpdateNotice_TruncatesLongSyncError(t *testing.T) {
	long := strings.Repeat("x", 5000)
	out := renderPluginUpdateNotice(PluginDrift{
		HasDrift: true, MirrorBehind: true,
		BinaryVersion: "0.4.6", MirrorVersion: "0.4.0", CacheVersion: "0.4.0",
		SyncError: long,
	})
	// Notice itself stays under ~600 chars even with a 5KB error.
	if len(out) > 800 {
		t.Errorf("notice ballooned to %d bytes; truncation regressed", len(out))
	}
	if !strings.Contains(out, "…") {
		t.Errorf("expected ellipsis after truncation, got: %s", out)
	}
}

// TestGitFastForward_TimesOut spins up a fake remote that hangs and
// verifies the 10s timeout fires. Skipped when git is missing or when
// the test env has no /dev/null-ish placeholder for askpass.
func TestGitFastForward_RejectsNonRepo(t *testing.T) {
	dir := t.TempDir() // no .git inside
	err := gitFastForward(dir)
	if err == nil || !strings.Contains(err.Error(), "not a git repo") {
		t.Fatalf("expected 'not a git repo', got %v", err)
	}
}

// TestTryAcquireSyncLock_Mutex confirms a second acquire on the same
// file fails fast (LOCK_NB returns EWOULDBLOCK). Critical for the
// "two Claude Code windows opening at once" race scenario. Skipped on
// Windows where the implementation is intentionally a permissive noop.
func TestTryAcquireSyncLock_Mutex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock is unix-only; Windows lock is a permissive noop")
	}
	// Redirect HOME so the test's lock file lives in a temp dir.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	release1, ok1 := tryAcquireSyncLock()
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	defer release1()

	_, ok2 := tryAcquireSyncLock()
	if ok2 {
		t.Fatal("second acquire should fail while first is held")
	}

	release1()
	release3, ok3 := tryAcquireSyncLock()
	if !ok3 {
		t.Fatal("third acquire (after release) should succeed")
	}
	release3()
}

func TestPluginManifestVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin.json")
	if err := os.WriteFile(path, []byte(`{"version":"0.4.6","name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := pluginManifestVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if v != "0.4.6" {
		t.Errorf("got %q, want 0.4.6", v)
	}
}

// seedMirrorManifest writes a minimal `.claude-plugin/plugin.json` under
// `mirrorDir` with the given version. Used to fake the marketplace mirror
// without cloning a real git repo.
func seedMirrorManifest(t *testing.T, mirrorDir, version string) {
	t.Helper()
	pj := filepath.Join(mirrorDir, ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(pj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pj, []byte(`{"name":"anchored","version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedPluginCache creates a fake `<cacheDir>/<version>/.claude-plugin/plugin.json`
// with a matching version field. Centralised so each test stays focused.
func seedPluginCache(t *testing.T, cacheDir, version string) {
	t.Helper()
	pj := filepath.Join(cacheDir, version, ".claude-plugin", "plugin.json")
	if err := os.MkdirAll(filepath.Dir(pj), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pj, []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v — %s", strings.Join(args, " "), err, string(out))
	}
}
