package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/updater"
)

// gitFastForwardTimeout caps how long the SessionStart hook can spend pulling
// the marketplace mirror. Unreachable remote, hung TCP, prompt for
// credentials — any of those would otherwise block Claude Code's launch
// indefinitely. Hooks are best-effort, so missing a sync is fine.
const gitFastForwardTimeout = 10 * time.Second

// syncLockPath is the advisory lock acquired before mutating the marketplace
// mirror or plugin cache. Concurrent SessionStart firings (two Claude Code
// windows opening at once) compete for this lock; whichever loses skips the
// sync silently and falls back to the manual-fix notice.
func syncLockPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".anchored", "plugin_sync.lock")
}

// PluginDrift describes two independent staleness signals between the
// running binary and the user's Claude Code plugin state:
//
//   - MirrorBehind: the marketplace git clone has not pulled the commits
//     that ship the current binary version. Anchored fixes this with a
//     fast-forward `git pull`.
//   - CacheBehind:  the installed plugin cache directory lags behind the
//     mirror (or is missing entirely). Anchored fixes this by copying the
//     mirror tree into the version-stamped cache dir AND rewriting Claude
//     Code's installed_plugins.json so the new version is loaded next
//     launch. The registry rewrite is gated on a schema check: if the
//     installed_plugins.json schema isn't the one we know how to write,
//     CacheInstalled stays false, CacheInstallError is populated, and we
//     fall back to a manual-fix notice so other plugins' state is never
//     corrupted.
//
// HasDrift = MirrorBehind || CacheBehind. Either implies the user is
// running with older hooks/skills than the binary expects.
type PluginDrift struct {
	BinaryVersion     string // binary's compile-time Version (set via ldflags)
	MirrorVersion     string // version field of mirror's plugin.json; "" when unreadable
	CacheVersion      string // newest semver dir under CacheDir; "" when absent
	HasDrift          bool
	MirrorBehind      bool // mirror lags binary (anchored can fix via git pull)
	CacheBehind       bool // cache lags mirror/binary
	MarketplaceDir    string
	CacheDir          string
	RegistryPath      string // resolved path to installed_plugins.json (for tests)
	SyncPerformed     bool   // git pull was actually run and succeeded
	SyncError         string // non-empty when git pull tried and failed
	CacheInstalled    bool   // mirror tree was copied to <cache>/<version> AND registry updated
	CacheInstallError string // non-empty when cache install tried and failed (schema mismatch, IO, etc.)
}

// detectPluginDrift compares the binary version with (a) the marketplace
// mirror's plugin.json and (b) the cache directory contents. Either being
// behind the binary counts as drift. The hook is best-effort: any IO or
// parse failure is silently treated as "no signal" rather than propagated.
func detectPluginDrift(cfg *config.Config, binaryVersion string) PluginDrift {
	d := PluginDrift{
		BinaryVersion:  binaryVersion,
		MarketplaceDir: cfg.Plugin.MarketplaceDir,
		CacheDir:       cfg.Plugin.CacheDir,
	}
	if binaryVersion == "" || updater.IsDevBuild(binaryVersion) {
		// "dev" placeholder = local `go build` without ldflags; dev-stamped
		// builds (make build) carry a git hash suffix. Drift comparison is
		// meaningless for either.
		return d
	}

	d.MirrorVersion = readMirrorPluginVersion(cfg.Plugin.MarketplaceDir)
	d.CacheVersion = newestInstalledVersion(cfg.Plugin.CacheDir)

	// MirrorBehind is the pull trigger: a binary newer than the mirror's plugin
	// version is a natural cue to fast-forward the marketplace mirror and pick
	// up any newer plugin upstream. It does NOT by itself mean the user must act
	// — the plugin is versioned on its own track, so the mirror routinely trails
	// the binary version even when fully up to date.
	if d.MirrorVersion != "" && compareSemver(d.MirrorVersion, binaryVersion) < 0 {
		d.MirrorBehind = true
	}
	// CacheBehind is the real, user-facing signal: the installed cache is older
	// than the marketplace mirror (the freshest plugin available locally), so
	// there are new hooks/skills to install. It is compared to the MIRROR, never
	// the binary — comparing to the binary reported a permanent false "plugin
	// lags the binary" on every release once the binary out-versioned the plugin.
	if d.MirrorVersion != "" && (d.CacheVersion == "" || compareSemver(d.CacheVersion, d.MirrorVersion) < 0) {
		d.CacheBehind = true
	}
	d.HasDrift = d.MirrorBehind || d.CacheBehind
	return d
}

// readMirrorPluginVersion extracts the version from the marketplace mirror's
// `.claude-plugin/plugin.json`. Returns "" if the mirror dir or file is
// missing, or if the version field is empty.
func readMirrorPluginVersion(mirrorDir string) string {
	if mirrorDir == "" {
		return ""
	}
	v, err := pluginManifestVersion(filepath.Join(mirrorDir, ".claude-plugin", "plugin.json"))
	if err != nil {
		return ""
	}
	return v
}

// newestInstalledVersion walks CacheDir for sub-directories whose name parses
// as a semver and returns the highest. Returns "" when the directory does
// not exist, no version dirs are present, or none parse cleanly.
func newestInstalledVersion(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return ""
	}
	var best string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		v := strings.TrimPrefix(e.Name(), "v")
		if !looksLikeSemver(v) {
			continue
		}
		// Confirm a plugin.json lives inside — protects against half-installed
		// or unrelated directories sharing the cache root.
		if _, err := os.Stat(filepath.Join(cacheDir, e.Name(), ".claude-plugin", "plugin.json")); err != nil {
			continue
		}
		if best == "" || compareSemver(best, v) < 0 {
			best = v
		}
	}
	return best
}

// looksLikeSemver requires EXACTLY three numeric segments before any optional
// "-prerelease" suffix. Strings like "0.4.6.extra", "1.2", "abc.def.ghi", and
// "0.4.6foo" are all rejected so compareSemver never sees garbage.
func looksLikeSemver(v string) bool {
	parts := strings.SplitN(v, "-", 2)
	dots := strings.Split(parts[0], ".")
	if len(dots) != 3 {
		return false
	}
	for _, d := range dots {
		if d == "" {
			return false
		}
		if _, err := strconv.Atoi(d); err != nil {
			return false
		}
	}
	return true
}

// compareSemver returns -1/0/1 for a < b / a == b / a > b. Only the X.Y.Z
// numeric part is compared; pre-release suffixes are ignored on purpose to
// avoid confusing rc1 < rc2 lexical edge cases when a release goes out.
func compareSemver(a, b string) int {
	pa := semverTriple(a)
	pb := semverTriple(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// semverTriple parses X.Y.Z into three ints via strconv.Atoi. Pre-release
// suffixes and stray characters yield 0 for that position (still total order
// preserving relative to clean versions). Overflow is handled by Atoi.
func semverTriple(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	v = strings.SplitN(v, "-", 2)[0]
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			// Best-effort: trim trailing non-digits so "6.extra" still
			// extracts 6 (matches the previous behavior).
			trimmed := parts[i]
			for j, r := range trimmed {
				if r < '0' || r > '9' {
					trimmed = trimmed[:j]
					break
				}
			}
			if trimmed == "" {
				out[i] = 0
				continue
			}
			n, _ = strconv.Atoi(trimmed)
		}
		out[i] = n
	}
	return out
}

// applyPluginAutoUpdate is the workhorse of the v0.4.10 auto-update flow.
// It performs, in order:
//
//  1. Acquire an advisory flock so two Claude Code windows don't race.
//  2. If MirrorBehind: `git pull --ff-only` the marketplace mirror.
//  3. If CacheBehind: copy the (now-fresh) mirror tree into
//     <cacheDir>/<mirrorVersion>/ and rewrite Claude Code's
//     installed_plugins.json to point at it.
//
// Step 3 is gated on a schema check (installed_plugins.json must be the
// version anchored knows how to write). Any failure there populates
// CacheInstallError and falls back to a manual-fix notice — anchored never
// corrupts other plugins' registry entries.
//
// Safety:
//   - `git pull --ff-only` runs under a 10s context timeout with
//     GIT_TERMINAL_PROMPT=0, GIT_ASKPASS=/bin/true, SSH_ASKPASS=/bin/true,
//     GIT_OPTIONAL_LOCKS=0 — unreachable remote, missing credential, or
//     askpass GUI cannot hang the SessionStart hook.
//   - File writes go through tmp+rename so a crash mid-install cannot leave
//     either the cache or the registry half-written.
//   - Existing cache dir at the destination is renamed to `<final>.bak`
//     before promotion; if promote fails we restore the backup.
//   - Failures become SyncError or CacheInstallError; the hook never aborts.
func applyPluginAutoUpdate(d PluginDrift) PluginDrift {
	if !d.MirrorBehind && !d.CacheBehind {
		return d
	}

	unlock, locked := tryAcquireSyncLock()
	if !locked {
		d.SyncError = "another anchored sync is in progress; skipping"
		return d
	}
	defer unlock()

	if d.MirrorBehind {
		if err := gitFastForward(d.MarketplaceDir); err != nil {
			d.SyncError = "git pull failed: " + err.Error()
			return d
		}
		d.SyncPerformed = true
		if v := readMirrorPluginVersion(d.MarketplaceDir); v != "" {
			d.MirrorVersion = v
		}
	}

	// Re-evaluate against the (possibly freshened) mirror: a pull may have
	// advanced MirrorVersion past the installed cache, which is exactly when a
	// reinstall is warranted. Comparing to the mirror — not the binary — avoids
	// reinstalling an already-current cache on every session.
	d.CacheBehind = d.MirrorVersion != "" &&
		(d.CacheVersion == "" || compareSemver(d.CacheVersion, d.MirrorVersion) < 0)

	if d.CacheBehind {
		registry := d.RegistryPath
		if registry == "" {
			registry = pluginRegistryPath()
		}
		targetVersion := d.MirrorVersion
		if targetVersion == "" {
			d.CacheInstallError = "mirror version unknown; cannot install"
			return d
		}
		if err := installPluginFromMirror(d.MarketplaceDir, d.CacheDir, registry, targetVersion); err != nil {
			d.CacheInstallError = err.Error()
			return d
		}
		d.CacheInstalled = true
		d.CacheVersion = targetVersion
	}
	return d
}

// tryAcquireSyncLock is OS-specific:
//   - plugin_sync_unix.go uses syscall.Flock (Linux + macOS + BSD)
//   - plugin_sync_windows.go is a permissive noop
//
// Both honor the same contract: return (releaseFn, true) when the caller
// holds an exclusive lock on ~/.anchored/plugin_sync.lock; (nop, false)
// when someone else is mutating the plugin cache.

func gitFastForward(dir string) error {
	if dir == "" {
		return fmt.Errorf("marketplace dir empty")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return fmt.Errorf("not a git repo: %s", dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitFastForwardTimeout)
	defer cancel()

	// Fast path: a clean mirror fast-forwards cleanly.
	if out, err := runGitCmd(ctx, dir, "pull", "--ff-only", "--quiet"); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timeout after %s", gitFastForwardTimeout)
		}
		// Recovery path: the marketplace mirror is a MANAGED, read-only cache.
		// A dirty worktree (e.g. a hand-edited hooks/hooks.json, plus a stray
		// .bak file) makes `--ff-only` fail and permanently pins the user to a
		// stale plugin version — the documented cause of "auto-sync failed:
		// local changes would be overwritten by merge". Since nothing in the
		// mirror is the user's to keep, hard-reset it to its upstream and retry
		// once. This is what makes "just update the binary" actually heal the
		// plugin install instead of wedging on a leftover local edit.
		if rerr := gitHardResetToUpstream(ctx, dir); rerr != nil {
			return fmt.Errorf("ff-only failed (%s); hard-reset recovery failed: %w",
				strings.TrimSpace(string(out)), rerr)
		}
		return nil
	}
	return nil
}

// gitHardResetToUpstream force-syncs a managed mirror to its tracked upstream,
// discarding any local commits, working-tree edits, and untracked files. It is
// ONLY called after `--ff-only` has already failed on a cache directory the
// user does not own, so the destructive reset is safe by construction. Best
// effort within the caller's context budget.
func gitHardResetToUpstream(ctx context.Context, dir string) error {
	if _, err := runGitCmd(ctx, dir, "fetch", "--quiet", "origin"); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("fetch timeout after %s", gitFastForwardTimeout)
		}
		return fmt.Errorf("fetch failed: %w", err)
	}
	// Prefer the configured upstream (@{u}); fall back to origin/HEAD then
	// origin/main so a mirror with no tracking ref still recovers.
	var lastErr error
	for _, ref := range []string{"@{u}", "origin/HEAD", "origin/main"} {
		if _, err := runGitCmd(ctx, dir, "reset", "--hard", "--quiet", ref); err == nil {
			// Drop the stray .bak / untracked files that also block ff-only.
			_, _ = runGitCmd(ctx, dir, "clean", "-fd", "--quiet")
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("reset --hard failed for all refs: %w", lastErr)
}

// runGitCmd runs a git subcommand in dir with a non-interactive environment so
// a missing credential never hangs the SessionStart hook, and returns combined
// output. Shared by the fast-forward and hard-reset recovery paths.
func runGitCmd(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Strip anything that could open an interactive prompt: no terminal
	// prompts, no SSH askpass GUI, no credential helper UI. If auth is
	// required and not pre-cached, we'd rather fail fast and tell the user
	// than hang the hook.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"SSH_ASKPASS=/bin/true",
		"GIT_OPTIONAL_LOCKS=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// renderPluginUpdateNotice builds an XML snippet for the SessionStart
// additionalContext when drift is detected. Shape mirrors the rest of the
// anchored bundle (XML-tagged sections) so the agent can route on a stable
// element name. The notice tells the user what anchored already did (git
// pull, when MirrorBehind+AutoUpdate) and what the user still needs to do
// (/plugin install + restart).
func renderPluginUpdateNotice(d PluginDrift) string {
	// Speak up only when the user must act: the installed cache trails the
	// mirror (new hooks to install/load) or a sync/install genuinely failed. A
	// mirror that merely trails the binary version — the steady state, since the
	// plugin is versioned on its own track — needs no action and stays silent.
	if !d.CacheBehind && !d.CacheInstalled && d.SyncError == "" && d.CacheInstallError == "" {
		return ""
	}

	cacheAttr := d.CacheVersion
	if cacheAttr == "" {
		cacheAttr = "absent"
	}

	var sb strings.Builder
	sb.WriteString("<anchored_plugin_update")
	fmt.Fprintf(&sb, " binary=%q mirror=%q cache=%q", d.BinaryVersion, d.MirrorVersion, cacheAttr)
	if d.SyncPerformed {
		sb.WriteString(" mirror_synced=\"true\"")
	}
	if d.CacheInstalled {
		sb.WriteString(" cache_installed=\"true\"")
	}
	sb.WriteString(">\n")

	switch {
	case d.CacheInstalled:
		// Best case: anchored did everything. User only needs to restart.
		fmt.Fprintf(&sb, "  Plugin auto-updated to v%s (mirror + cache + registry). ", escapeText(d.MirrorVersion))
		sb.WriteString("Restart Claude Code to load the new hooks.\n")
	case d.CacheInstallError != "":
		// Cache install attempted and refused (schema mismatch, IO error, etc.).
		// Mirror may already be synced; user finishes with /plugin install.
		fmt.Fprintf(&sb, "  Auto-install refused: %s\n", escapeText(truncateUTF8(d.CacheInstallError, 200)))
		sb.WriteString("  Fallback: run `/plugin install anchored@anchored` then restart Claude Code.\n")
	case d.SyncError != "":
		// Git pull failed; user drives the whole flow manually.
		fmt.Fprintf(&sb, "  Auto-sync failed: %s\n", escapeText(truncateUTF8(d.SyncError, 200)))
		sb.WriteString("  Manual fix: /plugin marketplace update anchored && /plugin install anchored@anchored — then restart Claude Code.\n")
	case d.SyncPerformed:
		// Mirror was just fast-forwarded; cache still needs Claude Code action.
		// (Only reachable when CacheBehind is false somehow, which shouldn't
		// happen since a fresh mirror at a new version implies the cache lags.)
		fmt.Fprintf(&sb, "  Marketplace mirror auto-synced to v%s.\n", escapeText(d.MirrorVersion))
		sb.WriteString("  Next: run `/plugin install anchored@anchored` then restart Claude Code to load the new hooks.\n")
	case d.MirrorBehind:
		// AutoUpdate is off (or path skipped install entirely).
		sb.WriteString("  Plugin is out of date. Run: /plugin marketplace update anchored && /plugin install anchored@anchored — then restart Claude Code.\n")
	default:
		// Only CacheBehind: mirror is current, cache lags. /plugin install is enough.
		sb.WriteString("  Plugin cache is stale. Run `/plugin install anchored@anchored` then restart Claude Code.\n")
	}
	sb.WriteString("</anchored_plugin_update>")
	return sb.String()
}

// truncateUTF8 caps a string at maxRunes runes, appending an ellipsis when
// truncated. Used on user-visible error messages to keep notices bounded.
func truncateUTF8(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes]) + "…"
}

// pluginManifestVersion reads the version field from a plugin.json. Used by
// tests; production code goes through newestInstalledVersion which is more
// resilient to half-installed states.
func pluginManifestVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	return doc.Version, nil
}
