package debuglog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// unsetEnv clears the variables that override the config; an empty
// ANCHORED_DEBUG means "force off", so they must be absent, not empty.
func unsetEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ANCHORED_DEBUG", "ANCHORED_DEBUG_PATH", "ANCHORED_DEBUG_CONTENT"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func openAt(t *testing.T, content bool) (*Logger, string) {
	t.Helper()
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	cfg := config.Defaults()
	cfg.Debug.Enabled = true
	cfg.Debug.Path = path
	cfg.Debug.Content = content
	l := Open(cfg)
	t.Cleanup(func() { _ = l.Close() })
	return l, path
}

const secretPrompt = "deploy with sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_ab please"

// Debug events carry prompt heads, tool arguments and outputs. By default the
// log records their size, never the text: a forgotten debug mode must not
// accumulate the user's prompts and pasted credentials on disk.
func TestEvent_RecordsOnlySizeByDefault(t *testing.T) {
	l, path := openAt(t, false)
	l.Event("hook.userpromptsubmit", map[string]any{"prompt_head": Content(secretPrompt, 240), "stage": "ok"})
	raw, _ := os.ReadFile(path)
	line := string(raw)
	if strings.Contains(line, "deploy with") || strings.Contains(line, "sk-ant") {
		t.Fatalf("content leaked into the log: %s", line)
	}
	if !strings.Contains(line, `"len":`) || !strings.Contains(line, `"stage":"ok"`) {
		t.Errorf("expected the size and the plain fields: %s", line)
	}
}

func TestEvent_ContentOptInIsRedacted(t *testing.T) {
	l, path := openAt(t, true)
	l.Event("hook.userpromptsubmit", map[string]any{"prompt_head": Content(secretPrompt, 240)})
	raw, _ := os.ReadFile(path)
	line := string(raw)
	if !strings.Contains(line, "deploy with") {
		t.Errorf("content mode must keep the text: %s", line)
	}
	if strings.Contains(line, "AbCdEfGh") {
		t.Errorf("content mode must still redact credentials: %s", line)
	}
}

func TestOpen_RotatesALargeLog(t *testing.T) {
	unsetEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.log")
	if err := os.WriteFile(path, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	// A current log (it has a session marker) rotates; one without is legacy.
	if err := os.WriteFile(path+sinceSuffix, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= keepRotated; i++ {
		if err := os.WriteFile(path+"."+string(rune('0'+i)), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Debug.Enabled = true
	cfg.Debug.Path = path
	l := Open(cfg)
	defer func() { _ = l.Close() }()
	l.Event("x", nil)

	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != maxLogBytes+1 {
		t.Errorf("the full log must move to .1: %v %v", info, err)
	}
	if info, _ := os.Stat(path); info.Size() > 1024 {
		t.Errorf("a fresh log must start after rotation, size %d", info.Size())
	}
	if _, err := os.Stat(path + "." + string(rune('0'+keepRotated+1))); err == nil {
		t.Errorf("only %d rotated files are kept", keepRotated)
	}
}

// A debug mode someone enabled to chase one problem and forgot keeps writing
// for months (71 MB on the machine that surfaced this). It stops after
// max_age_days unless ANCHORED_DEBUG forces it for the current process.
func TestOpen_DebugModeExpires(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	if err := os.WriteFile(path+sinceSuffix, []byte(time.Now().Add(-8*24*time.Hour).UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Debug.Enabled = true
	cfg.Debug.Path = path
	if l := Open(cfg); l.Enabled() {
		t.Error("a debug mode older than max_age_days must stop logging")
	}
	if st := Inspect(cfg); !st.Expired {
		t.Errorf("status must report the expiry: %+v", st)
	}

	t.Setenv("ANCHORED_DEBUG", "1")
	if l := Open(cfg); !l.Enabled() {
		t.Error("ANCHORED_DEBUG=1 forces logging for this process")
	} else {
		_ = l.Close()
	}
}

// Error strings repeat what the user sent (a URL with a token in its query, a
// path, an argument): they are logged scrubbed, never raw, in either mode.
func TestEvent_ScrubsPlainStringsLikeErrors(t *testing.T) {
	for _, content := range []bool{false, true} {
		l, path := openAt(t, content)
		l.Event("mcp.tool_call", map[string]any{
			"stage": "failed",
			"error": `fetch: executing request: Get "http://127.0.0.1:9/cb?access_token=sk-ant-api03-LEAKLEAKLEAKLEAKLEAK0123456789&code=SECRETOAUTHCODE": connection refused`,
			"cause": fmt.Errorf("open /tmp/x: %w", os.ErrNotExist),
		})
		raw, _ := os.ReadFile(path)
		line := string(raw)
		for _, leak := range []string{"LEAKLEAK", "SECRETOAUTHCODE", "access_token="} {
			if strings.Contains(line, leak) {
				t.Errorf("content=%v: %q leaked: %s", content, leak, line)
			}
		}
		for _, keep := range []string{"connection refused", "127.0.0.1:9/cb", `"stage":"failed"`, "file does not exist"} {
			if !strings.Contains(line, keep) {
				t.Errorf("content=%v: diagnostic %q lost: %s", content, keep, line)
			}
		}
	}
}

// A digest of a short prompt can be confirmed offline by hashing guesses, so
// the default mode records only the size.
func TestEvent_DefaultModeRecordsNoDigest(t *testing.T) {
	l, path := openAt(t, false)
	l.Event("x", map[string]any{"prompt_head": Content("yes", 240)})
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"sha"`) || !strings.Contains(string(raw), `"len":3`) {
		t.Errorf("default mode must record only the length: %s", raw)
	}
}

func enabledCfg(path string, maxAgeDays int) *config.Config {
	cfg := config.Defaults()
	cfg.Debug.Enabled = true
	cfg.Debug.Path = path
	cfg.Debug.MaxAgeDays = maxAgeDays
	return cfg
}

func TestOpen_StartsTheExpiryWindow(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	l := Open(enabledCfg(path, 7))
	defer func() { _ = l.Close() }()
	raw, err := os.ReadFile(path + sinceSuffix)
	if err != nil {
		t.Fatalf("opening an enabled log must record when the session started: %v", err)
	}
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil || time.Since(since) > time.Minute {
		t.Errorf("marker must hold the start time, got %q (%v)", raw, err)
	}
}

// Turning debug off in one process (ANCHORED_DEBUG=0, a config that failed
// to parse, a test run) must not renew the window of the one that has it on:
// otherwise the expiry never arrives.
func TestOpen_DisabledDoesNotResetTheWindow(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(path+sinceSuffix, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	disabled := enabledCfg(path, 7)
	disabled.Debug.Enabled = false
	_ = Open(disabled).Close()
	if raw, _ := os.ReadFile(path + sinceSuffix); string(raw) != old {
		t.Fatalf("a disabled open must leave the marker alone, got %q", raw)
	}
	if l := Open(enabledCfg(path, 7)); l.Enabled() {
		t.Error("the expired window must still be expired")
	}
}

func TestOpen_RotationShiftsGenerations(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	for i, content := range []string{"gen1", "gen2", "gen3"} {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", path, i+1), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path+sinceSuffix, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The current log is over the limit.
	if err := os.WriteFile(path, append([]byte("gen0"), make([]byte, maxLogBytes)...), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = Open(enabledCfg(path, 7)).Close()
	for i, want := range []string{"gen0", "gen1", "gen2"} {
		raw, _ := os.ReadFile(fmt.Sprintf("%s.%d", path, i+1))
		if !strings.HasPrefix(string(raw), want) {
			t.Errorf(".%d must hold %s, got %q", i+1, want, head(raw))
		}
	}
}

func head(b []byte) string {
	if len(b) > 8 {
		return string(b[:8])
	}
	return string(b)
}

// A server process opens the log once and lives for days: it must stop at
// the expiry and follow rotations without reopening.
func TestEvent_LongLivedProcessExpiresAndRotates(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	clock := time.Now()
	restore := setClock(func() time.Time { return clock })
	defer restore()

	l := Open(enabledCfg(path, 7))
	defer func() { _ = l.Close() }()
	l.Event("before", nil)

	// Another process rotates the file away.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rotationCheckEvery+1; i++ {
		l.Event("after-rotation", nil)
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "after-rotation") {
		t.Errorf("writes after a rotation must reach the new file: %v", err)
	}

	clock = clock.Add(8 * 24 * time.Hour)
	l.Event("after-expiry", nil)
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "after-expiry") {
		t.Error("a long-lived process must stop logging at the expiry")
	}
}

// A log written before this version (no marker) holds raw prompts and tool
// output; it is moved aside, private, instead of being rotated into .1.
func TestOpen_SetsALegacyLogAside(t *testing.T) {
	unsetEnv(t)
	path := filepath.Join(t.TempDir(), "debug.log")
	if err := os.WriteFile(path, []byte(`{"prompt_head":"raw text"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	l := Open(enabledCfg(path, 7))
	defer func() { _ = l.Close() }()
	info, err := os.Stat(path + legacySuffix)
	if err != nil {
		t.Fatalf("the pre-redaction log must be set aside: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the set-aside log must be private, got %o", info.Mode().Perm())
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "raw text") {
		t.Error("the new log must start empty")
	}
	if st := InspectAt(enabledCfg(path, 7), path); !st.Legacy {
		t.Errorf("status must report the legacy log: %+v", st)
	}
}

// The log and its marker are opened without following symlinks: a link planted
// at either path must not redirect events, the marker's truncating write, a
// rotation or a chmod into the file it names.
func TestOpen_DoesNotWriteThroughASymlink(t *testing.T) {
	unsetEnv(t)
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "debug.log")
	for _, link := range []string{path, path + sinceSuffix} {
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
	}
	old := maxLogBytes
	maxLogBytes = 1 // the victim is "large": rotation must still leave it alone
	t.Cleanup(func() { maxLogBytes = old })

	cfg := config.Defaults()
	cfg.Debug.Enabled = true
	cfg.Debug.Path = path
	l := Open(cfg)
	t.Cleanup(func() { _ = l.Close() })
	l.Event("hook.test", map[string]any{"stage": "ok"})

	got, _ := os.ReadFile(victim)
	info, _ := os.Stat(victim)
	if string(got) != "keep\n" || info.Mode().Perm() != 0o644 {
		t.Fatalf("the symlink target was touched: %q mode %o", got, info.Mode().Perm())
	}
}
