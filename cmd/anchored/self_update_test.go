package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/updater"
)

func TestSelfUpdateExitCode(t *testing.T) {
	cases := []struct {
		name string
		res  updater.Result
		want int
	}{
		{
			name: "applicable update",
			res:  updater.Result{Current: "0.17.0", Latest: "0.18.0", Newer: true},
			want: exitUpdateAvailable,
		},
		{
			name: "already latest",
			res:  updater.Result{Current: "0.18.0", Latest: "0.18.0", Blocked: updater.BlockNotNewer},
			want: 0,
		},
		// A blocked check exits 0: nothing will happen without --force, so a
		// script polling for "there is work to do" must not be woken up by a
		// refusal it cannot act on.
		{
			name: "blocked on dev build even with a newer release out",
			res:  updater.Result{Current: "0.17.0-dev+gabc", Latest: "0.18.0", Newer: true, Blocked: updater.BlockDevBuild},
			want: 0,
		},
		{
			name: "blocked outside canonical dir",
			res:  updater.Result{Current: "0.17.0", Latest: "0.18.0", Newer: true, Blocked: updater.BlockOutsideCanonical},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selfUpdateExitCode(tc.res); got != tc.want {
				t.Fatalf("selfUpdateExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// Every refusal must name itself and say what overrides it; a report that
// says "blocked" without the cause is the silent no-op this command exists
// to replace.
func TestRenderSelfUpdateCheck_ExplainsEveryBlockReason(t *testing.T) {
	cases := []struct {
		reason   updater.BlockReason
		mustHave []string
	}{
		{updater.BlockDevBuild, []string{"dev", "--force"}},
		{updater.BlockOutsideCanonical, []string{".anchored/bin", "--force"}},
		{updater.BlockEnvDisabled, []string{"ANCHORED_NO_AUTOUPDATE", "--force"}},
		{updater.BlockNotNewer, []string{"0.18.0"}},
		{updater.BlockNoVersion, []string{"ldflags"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			out := renderSelfUpdateCheckT(updater.Result{
				Current: "0.17.0-dev+gabc",
				Latest:  "0.18.0",
				BinPath: "/home/u/.anchored/bin/anchored",
				Blocked: tc.reason,
			})
			for _, want := range tc.mustHave {
				if !strings.Contains(out, want) {
					t.Errorf("report for %q missing %q\n---\n%s", tc.reason, want, out)
				}
			}
		})
	}
}

func TestRenderSelfUpdateCheck_ShowsVersionsAndPath(t *testing.T) {
	out := renderSelfUpdateCheckT(updater.Result{
		Current: "0.17.0",
		Latest:  "0.18.0",
		BinPath: "/home/u/.anchored/bin/anchored",
		Newer:   true,
	})
	for _, want := range []string{"0.17.0", "0.18.0", "/home/u/.anchored/bin/anchored"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderSelfUpdateCheck_UnknownLatestWhenUnresolved(t *testing.T) {
	out := renderSelfUpdateCheckT(updater.Result{
		Current: "0.17.0",
		BinPath: "/home/u/.anchored/bin/anchored",
		Blocked: updater.BlockEnvDisabled,
	})
	if strings.Contains(out, "→ \n") {
		t.Errorf("empty latest rendered as a dangling arrow\n---\n%s", out)
	}
}

func TestRenderSelfUpdateJSON(t *testing.T) {
	raw := renderSelfUpdateJSON(updater.Result{
		Current: "0.17.0-dev+gabc",
		Latest:  "0.18.0",
		BinPath: "/home/u/.anchored/bin/anchored",
		Newer:   true,
		Blocked: updater.BlockDevBuild,
	})

	var got struct {
		Current         string `json:"current"`
		Latest          string `json:"latest"`
		BinPath         string `json:"bin_path"`
		UpdateAvailable bool   `json:"update_available"`
		Blocked         string `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
	}
	if got.Current != "0.17.0-dev+gabc" || got.Latest != "0.18.0" {
		t.Errorf("versions = %q / %q", got.Current, got.Latest)
	}
	if got.BinPath != "/home/u/.anchored/bin/anchored" {
		t.Errorf("bin_path = %q", got.BinPath)
	}
	// update_available reports what the command would actually DO, so a
	// blocked result is false even though a newer release exists — that is
	// the distinction the exit code cannot carry.
	if got.UpdateAvailable {
		t.Error("update_available should be false when blocked")
	}
	if got.Blocked != string(updater.BlockDevBuild) {
		t.Errorf("blocked = %q, want %q", got.Blocked, updater.BlockDevBuild)
	}
}

func TestSelfUpdateCurrentVersion_StripsLeadingV(t *testing.T) {
	// Version comes from ldflags as "v0.17.0"; updater compares numerically
	// and Atoi("v0") yields 0, so the "v" has to come off before comparison.
	if got := selfUpdateCurrentVersion("v0.18.0"); got != "0.18.0" {
		t.Fatalf("got %q, want 0.18.0", got)
	}
	if got := selfUpdateCurrentVersion("0.18.0"); got != "0.18.0" {
		t.Fatalf("got %q, want 0.18.0", got)
	}
}

func TestSelfUpdateIsRegisteredInUsage(t *testing.T) {
	out := captureUsage(t)
	if !strings.Contains(out, "self-update") {
		t.Errorf("printUsage does not mention self-update:\n%s", out)
	}
	// `update` is memory update; the two are one keystroke apart and the
	// usage text is the only place a user learns which is which.
	if !strings.Contains(out, "Update a memory") {
		t.Errorf("printUsage lost the memory-update line:\n%s", out)
	}
}

// captureUsage collects what printUsage writes to stderr.
func captureUsage(t *testing.T) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	printUsage()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestEnsureWritable_AcceptsWritableTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anchored")
	if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureWritable(path); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// The swap renames within the parent dir, so write permission on the DIR is
// what actually matters — a writable file inside a read-only dir still fails.
func TestEnsureWritable_RejectsReadOnlyParentDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "anchored")
	if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Errorf("restore dir perms: %v", err)
		}
	})

	err := ensureWritable(path)
	if err == nil {
		t.Fatal("expected an error for a read-only parent dir")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error should name the directory, got %v", err)
	}
}

func TestEnsureWritable_ChecksParentWhenFileAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := ensureWritable(filepath.Join(dir, "not-there-yet")); err != nil {
		t.Fatalf("a fresh install into a writable dir must pass, got %v", err)
	}
}

func TestEnsureWritable_LeavesNoProbeBehind(t *testing.T) {
	dir := t.TempDir()
	if err := ensureWritable(filepath.Join(dir, "anchored")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe leaked: %v", entries)
	}
}

// The hint is printed for the user to paste into a root shell, so it is
// rebuilt from parsed flags. Echoing argv would let a version string carry a
// second command into that paste.
func TestSudoHint_BuildsFromParsedFlags(t *testing.T) {
	got := sudoHint("/usr/local/bin/anchored", selfUpdateFlags(true, true, false, "", ""))
	want := "sudo '/usr/local/bin/anchored' self-update --force --yes"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// The hint can only ever render a --version value that already passed
// updater.releaseTag's shape check, because Check rejects a hostile one and
// the command exits before any writability probe runs. This asserts the
// piece that lives here: the hint is assembled from known flags, never from
// os.Args, so nothing else the user typed can reach it.
func TestSudoHint_AssemblesOnlyKnownFlags(t *testing.T) {
	got := sudoHint("/usr/local/bin/anchored", selfUpdateFlags(true, false, true, "", "v0.17.0"))
	want := "sudo '/usr/local/bin/anchored' self-update --force --no-plugin --version v0.17.0"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderSelfUpdateInstalled(t *testing.T) {
	out := renderSelfUpdateInstalled(updater.Result{
		Current: "0.17.0",
		Latest:  "0.18.0",
		BinPath: "/home/u/.anchored/bin/anchored",
	})
	for _, want := range []string{"0.17.0", "0.18.0", "/home/u/.anchored/bin/anchored", ".prev"} {
		if !strings.Contains(out, want) {
			t.Errorf("success report missing %q\n---\n%s", want, out)
		}
	}
	// A swapped binary does not reach a running MCP server; saying so is the
	// difference between "it worked" and "it worked and you must restart".
	if !strings.Contains(strings.ToLower(out), "restart") {
		t.Errorf("success report does not tell the user to restart\n---\n%s", out)
	}
}

// Only the refusals a user can legitimately decide against are overridable.
// An unrecognized reason must keep refusing, so a future guard is not
// silently bypassed by a flag that predates it.
func TestForceOverridable(t *testing.T) {
	overridable := []updater.BlockReason{
		updater.BlockDevBuild,
		updater.BlockOutsideCanonical,
		updater.BlockEnvDisabled,
		updater.BlockNotNewer,
		updater.BlockNoVersion,
	}
	for _, r := range overridable {
		if !forceOverridable(r) {
			t.Errorf("%q should be overridable by --force", r)
		}
	}
	if forceOverridable(updater.BlockReason("some-future-guard")) {
		t.Error("an unknown reason must not be overridable")
	}
	if forceOverridable(updater.BlockNone) {
		t.Error("BlockNone is not a refusal")
	}
}

func TestConfirmDevBuildOverwrite_ProceedsOnYes(t *testing.T) {
	var out strings.Builder
	ok, err := confirmDevBuildOverwrite(devBuildResult(), strings.NewReader("y\n"), &out, false, true)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ok {
		t.Fatal("y should proceed")
	}
}

func TestConfirmDevBuildOverwrite_DefaultsToNo(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "no\n", "whatever\n"} {
		var out strings.Builder
		ok, err := confirmDevBuildOverwrite(devBuildResult(), strings.NewReader(answer), &out, false, true)
		if err != nil {
			t.Fatalf("answer %q: unexpected err: %v", answer, err)
		}
		if ok {
			t.Errorf("answer %q should not proceed", answer)
		}
	}
}

func TestConfirmDevBuildOverwrite_AssumeYesSkipsThePrompt(t *testing.T) {
	var out strings.Builder
	ok, err := confirmDevBuildOverwrite(devBuildResult(), strings.NewReader(""), &out, true, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ok {
		t.Fatal("--yes should proceed")
	}
	if out.Len() != 0 {
		t.Errorf("--yes should not print a prompt, got %q", out.String())
	}
}

// Without a TTY there is nobody to answer, so consent cannot be assumed.
func TestConfirmDevBuildOverwrite_AbortsWithoutTTY(t *testing.T) {
	var out strings.Builder
	ok, err := confirmDevBuildOverwrite(devBuildResult(), strings.NewReader(""), &out, false, false)
	if ok {
		t.Fatal("must not proceed without a TTY and without --yes")
	}
	if err == nil {
		t.Fatal("expected an error explaining how to proceed")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error should point at --yes, got %v", err)
	}
}

func TestConfirmDevBuildOverwrite_PromptNamesTheStakes(t *testing.T) {
	var out strings.Builder
	if _, err := confirmDevBuildOverwrite(devBuildResult(), strings.NewReader("n\n"), &out, false, true); err != nil {
		t.Fatal(err)
	}
	prompt := out.String()
	// The user is about to lose a local build; the prompt has to say what is
	// being replaced, with what, and where the old one goes.
	for _, want := range []string{"0.17.0-dev+gabc", "0.18.0", ".prev"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

func devBuildResult() updater.Result {
	return updater.Result{
		Current: "0.17.0-dev+gabc",
		Latest:  "0.18.0",
		BinPath: "/home/u/.anchored/bin/anchored",
		Newer:   true,
		Blocked: updater.BlockDevBuild,
	}
}

// The dev-build guard is what froze the plugin at 0.17.0 while releases moved
// on. An explicit request has to be able to read the state it hides.
func TestDetectPluginDriftWithForce_IgnoresTheDevBuildGuard(t *testing.T) {
	cacheDir := t.TempDir()
	mirrorDir := t.TempDir()
	seedPluginCache(t, cacheDir, "0.17.0")
	seedMirrorManifest(t, mirrorDir, "0.18.0")

	cfg := &config.Config{}
	cfg.Plugin.CacheDir = cacheDir
	cfg.Plugin.MarketplaceDir = mirrorDir

	if d := detectPluginDrift(cfg, "v0.17.0-dev+gabc"); d.MirrorVersion != "" || d.HasDrift {
		t.Fatalf("guarded detection should still see nothing on a dev build, got %+v", d)
	}

	f := detectPluginDriftWithForce(cfg, "v0.17.0-dev+gabc", true)
	if f.MirrorVersion != "0.18.0" || f.CacheVersion != "0.17.0" {
		t.Fatalf("forced detection did not read the versions: %+v", f)
	}
	// On an explicit request the mirror is always worth refreshing: the point
	// is to fetch the newest plugin, not to infer it from a version stamp
	// that cannot be compared. MirrorBehind is the field applyPluginAutoUpdate
	// actually reads — it recomputes CacheBehind itself.
	if !f.MirrorBehind {
		t.Error("forced detection should always try to refresh the mirror")
	}
}

// The bug this catches: syncPluginAfterUpdate runs AFTER the binary was
// swapped, so reading the package-level Version yields the release this
// process was compiled as — the old one. Drift then compares the mirror
// against the version that no longer exists on disk, finds nothing behind,
// and prints "already current" while leaving the plugin exactly as stale.
func TestDetectPluginDrift_MeasuresAgainstTheInstalledVersion(t *testing.T) {
	cacheDir := t.TempDir()
	mirrorDir := t.TempDir()
	seedPluginCache(t, cacheDir, "0.17.0")
	seedMirrorManifest(t, mirrorDir, "0.17.0")

	cfg := &config.Config{}
	cfg.Plugin.CacheDir = cacheDir
	cfg.Plugin.MarketplaceDir = mirrorDir

	// Reading the pre-swap version: mirror 0.17.0 vs binary 0.17.0, nothing
	// looks behind, so no refresh is attempted.
	if d := detectPluginDrift(cfg, "0.17.0"); d.MirrorBehind {
		t.Fatal("precondition: mirror level with the old version should look current")
	}
	// Reading what is now on disk: the mirror trails it, so a refresh runs.
	if d := detectPluginDrift(cfg, "0.18.0"); !d.MirrorBehind {
		t.Error("mirror 0.17.0 against installed 0.18.0 must trigger a refresh")
	}
}

func TestRenderPluginSyncOutcome_NamesTheNoOpPath(t *testing.T) {
	out := renderPluginSyncOutcome(pluginSyncOutcome{
		MarketplaceDir:     "/home/u/.claude/plugins/marketplaces/anchored",
		MarketplaceMissing: true,
	})
	if !strings.Contains(out, "/home/u/.claude/plugins/marketplaces/anchored") {
		t.Errorf("outcome must name the path it looked at\n---\n%s", out)
	}
	// The config default and this machine's real marketplace root have
	// differed before; exiting 0 having silently done nothing is the failure
	// mode worth shouting about.
	if !strings.Contains(strings.ToLower(out), "not") {
		t.Errorf("outcome must say plainly that nothing was done\n---\n%s", out)
	}
}

func TestRenderPluginSyncOutcome_ReportsAnInstall(t *testing.T) {
	out := renderPluginSyncOutcome(pluginSyncOutcome{
		MarketplaceDir: "/m",
		CacheDir:       "/c",
		Drift: PluginDrift{
			MirrorVersion:  "0.18.0",
			CacheVersion:   "0.18.0",
			SyncPerformed:  true,
			CacheInstalled: true,
		},
	})
	if !strings.Contains(out, "0.18.0") {
		t.Errorf("outcome should name the installed plugin version\n---\n%s", out)
	}
}

// A plugin failure must not read as "the update failed" — the binary was
// already replaced by then.
func TestRenderPluginSyncOutcome_FailureKeepsTheBinaryUpdate(t *testing.T) {
	out := renderPluginSyncOutcome(pluginSyncOutcome{
		MarketplaceDir: "/m",
		CacheDir:       "/c",
		Drift:          PluginDrift{MirrorBehind: true, SyncError: "git pull failed: no upstream"},
	})
	if !strings.Contains(out, "no upstream") {
		t.Errorf("outcome should carry the underlying error\n---\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "binary") {
		t.Errorf("outcome should say the binary update still stands\n---\n%s", out)
	}
}

func TestRenderPluginSyncOutcome_SkippedIsSilent(t *testing.T) {
	if out := renderPluginSyncOutcome(pluginSyncOutcome{Skipped: true}); out != "" {
		t.Errorf("--no-plugin should print nothing, got %q", out)
	}
}

// Exercises the real function, config load included, for the case the plan
// flagged as the silent failure: a configured marketplace that is not there.
func TestSyncPluginAfterUpdate_MissingMarketplaceIsReportedNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	absent := filepath.Join(dir, "not-a-marketplace")
	body := "plugin:\n  marketplace_dir: " + absent + "\n  cache_dir: " + filepath.Join(dir, "cache") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out := syncPluginAfterUpdate(cfgPath, updater.Result{Latest: "0.18.0"}, false, true)
	if !out.MarketplaceMissing {
		t.Fatalf("missing marketplace not detected: %+v", out)
	}
	if out.MarketplaceDir != absent {
		t.Errorf("MarketplaceDir = %q, want %q", out.MarketplaceDir, absent)
	}
	if rendered := renderPluginSyncOutcome(out); !strings.Contains(rendered, absent) {
		t.Errorf("report does not name the missing path\n---\n%s", rendered)
	}
}

func TestSyncPluginAfterUpdate_NoPluginSkipsEverything(t *testing.T) {
	out := syncPluginAfterUpdate("", updater.Result{Latest: "0.18.0"}, true, true)
	if !out.Skipped {
		t.Fatal("--no-plugin must skip")
	}
	if out.MarketplaceDir != "" {
		t.Errorf("skipped sync should not resolve paths, got %q", out.MarketplaceDir)
	}
}

// The doctor closes the discovery loop: you learn you are behind from the
// diagnostic you already run, and it hands you the command.
func TestReleaseCheckResult_ReportsAnAvailableUpdate(t *testing.T) {
	status, detail, fix := releaseCheckResult(updater.Result{
		Current: "0.17.0", Latest: "0.18.0", Newer: true,
	}, nil)
	if status == "ok" {
		t.Errorf("an available update should not read as ok, got %q", status)
	}
	if !strings.Contains(detail, "0.18.0") {
		t.Errorf("detail should name the release, got %q", detail)
	}
	if !strings.Contains(fix, "self-update") {
		t.Errorf("fix should hand over the command, got %q", fix)
	}
}

func TestReleaseCheckResult_OKWhenCurrent(t *testing.T) {
	status, _, _ := releaseCheckResult(updater.Result{
		Current: "0.18.0", Latest: "0.18.0", Blocked: updater.BlockNotNewer,
	}, nil)
	if status != "ok" {
		t.Errorf("status = %q, want ok", status)
	}
}

// No network must degrade, never fail: a doctor that goes red on a plane is
// a doctor people stop running.
func TestReleaseCheckResult_DegradesWhenUnreachable(t *testing.T) {
	status, detail, _ := releaseCheckResult(updater.Result{Current: "0.18.0"}, errors.New("dial tcp: no route to host"))
	if status != "skipped" {
		t.Errorf("status = %q, want skipped", status)
	}
	if !strings.Contains(strings.ToLower(detail), "not checked") {
		t.Errorf("detail should say it was not checked, got %q", detail)
	}
}

// A dev build is not "behind" in a way the doctor should nag about, but it
// should still say a release exists.
func TestReleaseCheckResult_MentionsReleaseOnDevBuild(t *testing.T) {
	status, detail, _ := releaseCheckResult(updater.Result{
		Current: "0.17.0-dev+gabc", Latest: "0.18.0", Newer: true, Blocked: updater.BlockDevBuild,
	}, nil)
	if status == "failed" {
		t.Error("a dev build is a deliberate state, not a failure")
	}
	if !strings.Contains(detail, "0.18.0") {
		t.Errorf("detail should still name the release, got %q", detail)
	}
}

// A downgrade must not read as an update. "Installed v0.16.0 (was v0.18.0)"
// is technically true and completely misleading.
func TestRenderSelfUpdateInstalled_NamesADowngrade(t *testing.T) {
	out := renderSelfUpdateInstalled(updater.Result{
		Current: "0.18.0",
		Latest:  "0.16.0",
		BinPath: "/home/u/.anchored/bin/anchored",
		Newer:   false,
	})
	if !strings.Contains(strings.ToUpper(out), "DOWNGRAD") {
		t.Errorf("a downgrade must say so\n---\n%s", out)
	}
	forward := renderSelfUpdateInstalled(updater.Result{
		Current: "0.17.0", Latest: "0.18.0", BinPath: "/b", Newer: true,
	})
	if strings.Contains(strings.ToUpper(forward), "DOWNGRAD") {
		t.Errorf("a normal update must not\n---\n%s", forward)
	}
}

func TestRenderDowngradeRefusal_ExplainsItselfAndTheOverride(t *testing.T) {
	out := renderDowngradeRefusal(updater.Result{Current: "0.18.0", Latest: "0.16.0"})
	for _, want := range []string{"0.16.0", "0.18.0", "--force"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal missing %q\n---\n%s", want, out)
		}
	}
	// The old wording claimed the older version was already installed.
	if strings.Contains(out, "Up to date") {
		t.Errorf("a requested downgrade is not 'up to date'\n---\n%s", out)
	}
}

// renderSelfUpdateCheckT keeps the existing render assertions readable now
// that the renderer needs to know whether a version was pinned.
func renderSelfUpdateCheckT(res updater.Result) string {
	return renderSelfUpdateCheck(res, "")
}

// Blocked must be BlockNotNewer here: check.go always sets it when the
// resolved release is not newer, so a downgrade with BlockNone is a state the
// code cannot produce — asserting on it hid the bug this now covers.
func TestRenderSelfUpdateCheck_PinnedDowngradeIsNotReportedAsUpToDate(t *testing.T) {
	out := renderSelfUpdateCheck(updater.Result{
		Current: "0.18.0",
		Latest:  "0.16.0",
		BinPath: "/b",
		Blocked: updater.BlockNotNewer,
	}, "v0.16.0")

	if strings.Contains(out, "Up to date") {
		t.Errorf("a requested downgrade is not 'up to date'\n---\n%s", out)
	}
	if !strings.Contains(out, "target") {
		t.Errorf("a pinned version must be labelled target, not latest\n---\n%s", out)
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("the refusal must name the override\n---\n%s", out)
	}
}

// The override command must carry the flags the user gave. Telling someone who
// asked for v0.16.0 to run plain --force sends them to the latest release.
func TestOverrideCommand_PreservesThePinnedVersion(t *testing.T) {
	got := overrideCommand("v0.16.0")
	if !strings.Contains(got, "--version v0.16.0") {
		t.Fatalf("override command dropped the pin: %q", got)
	}
	if plain := overrideCommand(""); strings.Contains(plain, "--version") {
		t.Fatalf("no pin should mean no --version: %q", plain)
	}
}

func TestReleaseCheckResult_UsesAStatusDoctorCanRender(t *testing.T) {
	status, _, _ := releaseCheckResult(updater.Result{
		Current: "0.17.0", Latest: "0.18.0", Newer: true,
	}, nil)
	// recordCheck has no "failed" arm, so that status renders as a blank box.
	if status == "failed" {
		t.Error(`"failed" has no render arm in recordCheck; use "warn"`)
	}
	if status != "warn" {
		t.Errorf("status = %q, want warn", status)
	}
}

// Pins the call site, not just the helper: reverting to the package-level
// Version — the original defect — must fail a test. Version is deliberately
// set to the OLD release here, which is exactly what a running process
// reports after its own binary has been swapped.
func TestSyncPluginAfterUpdate_UsesTheResolvedReleaseNotTheCompiledVersion(t *testing.T) {
	origVersion := Version
	Version = "0.17.0"
	t.Cleanup(func() { Version = origVersion })

	dir := t.TempDir()
	mirrorDir := filepath.Join(dir, "mirror")
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(mirrorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seedMirrorManifest(t, mirrorDir, "0.17.0")
	seedPluginCache(t, cacheDir, "0.17.0")

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "plugin:\n  marketplace_dir: " + mirrorDir + "\n  cache_dir: " + cacheDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out := syncPluginAfterUpdate(cfgPath, updater.Result{Latest: "0.18.0"}, false, false)
	// Mirror 0.17.0 against the installed 0.18.0 is behind, so a refresh is
	// attempted. Reading Version ("0.17.0") would find nothing behind.
	if !out.Drift.MirrorBehind {
		t.Fatalf("drift was measured against the compiled version, not the installed release: %+v", out.Drift)
	}
}

// A path with a space is legitimate and must not split the pasted command.
func TestSudoHint_QuotesThePath(t *testing.T) {
	got := sudoHint("/Users/Jo Smith/.anchored/bin/anchored", nil)
	if !strings.Contains(got, "'/Users/Jo Smith/.anchored/bin/anchored'") {
		t.Fatalf("path not quoted: %q", got)
	}
}

// --json in apply mode used to be ignored silently, which is the worst of the
// three possible behaviours: the flag is accepted, the contract is not
// honoured, and nothing says so.
func TestRenderApplyJSON_BranchableAction(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  applyOutcome
		want string
	}{
		{"installed", applyOutcome{Action: "installed", Current: "0.17.0", Latest: "0.18.0", BinPath: "/b", Previous: "/b.prev"}, "installed"},
		{"already current", applyOutcome{Action: "already_current", Current: "0.18.0"}, "already_current"},
		{"refused", applyOutcome{Action: "refused", Blocked: "dev-build", Override: "anchored self-update --force"}, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			if err := json.Unmarshal([]byte(renderApplyJSON(tc.out)), &got); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}
			if got["action"] != tc.want {
				t.Errorf("action = %v, want %q", got["action"], tc.want)
			}
		})
	}
}

// Absent fields must be omitted rather than shipped empty, so a consumer can
// tell "no plugin was touched" from "the plugin failed".
func TestRenderApplyJSON_OmitsWhatDidNotHappen(t *testing.T) {
	raw := renderApplyJSON(applyOutcome{Action: "already_current", Current: "0.18.0"})
	for _, absent := range []string{"plugin", "blocked", "override", "bin_path"} {
		if strings.Contains(raw, absent) {
			t.Errorf("%q should be omitted: %s", absent, raw)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "third"); got != "third" {
		t.Errorf("got %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// Check reports only the first refusal and evaluates the env kill switch
// before the dev-build guard, so keying the prompt on res.Blocked let
// ANCHORED_NO_AUTOUPDATE=1 — what someone working from a checkout sets —
// overwrite a dev build with no question asked.
func TestConfirmationIsKeyedOnTheBuildNotTheRefusal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	bin := filepath.Join(dir, ".anchored", "bin", "anchored")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANCHORED_NO_AUTOUPDATE", "1")
	res, err := updater.Check(context.Background(), updater.Options{
		CurrentVersion: "0.17.0-dev+gabc",
		BinPath:        bin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocked != updater.BlockEnvDisabled {
		t.Fatalf("precondition: the env guard should win the race, got %q", res.Blocked)
	}
	// The old gate compared res.Blocked to BlockDevBuild and so was false here.
	if !updater.IsDevBuild(res.Current) {
		t.Fatal("the current version is a dev build and must still be treated as one")
	}
}
