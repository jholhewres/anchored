// Package debuglog provides a lightweight, opt-in NDJSON event logger for
// auditing how anchored is actually being used by the host AI tool.
//
// The goal is observability, not telemetry: when the user wants to understand
// why memory tools fired (or didn't), they enable Debug.Enabled in
// ~/.anchored/config.yaml (or set ANCHORED_DEBUG=1) and get an append-only
// NDJSON file they can grep, jq, or replay.
//
// Design rules:
//   - Never fail the caller. Open/Write errors are swallowed; the worst case
//     is a missing log line, never a broken hook or MCP call.
//   - One JSON object per line ({ts, event, ...fields}) so the file is
//     trivially parsable with jq -c or Python's json.loads per line.
//   - Append-only with O_APPEND so concurrent processes (multiple hook
//     invocations + the MCP server) don't clobber each other; we still take a
//     mutex per *Logger to keep individual writes intact.
package debuglog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/redact"
)

const (
	keepRotated = 3
	// sinceSuffix names the marker holding when this debug session started,
	// which max_age_days is measured from. Only the user removes it (to renew
	// the window): a process with debug off must not reset another's clock.
	sinceSuffix = ".since"
	// legacySuffix is where a log written before redaction existed (no
	// marker next to it) is moved, so its raw content is not carried into
	// the rotation.
	legacySuffix = ".legacy"
	// rotationCheckEvery is how many writes a long-lived logger makes between
	// checks for rotation (size, or the file renamed by another process).
	rotationCheckEvery = 256
)

// maxLogBytes is the size at which the log rotates; keepRotated older files
// (debug.log.1 … .3) are kept.
var maxLogBytes int64 = 10 << 20

// now is the logger's clock; tests move it to cross the expiry.
var now = time.Now

func setClock(f func() time.Time) (restore func()) {
	prev := now
	now = f
	return func() { now = prev }
}

// Logger is a single-file NDJSON appender. The zero value is a valid no-op
// Logger; callers can safely call Event on a nil receiver.
type Logger struct {
	mu      sync.Mutex
	file    *os.File
	enabled bool
	path    string
	content bool
	// expires ends logging in a process that outlives max_age_days (the MCP
	// server opens the log once); zero means no limit.
	expires time.Time
	writes  int
}

// Text is user text (a prompt, tool arguments, output) handed to Event. The
// logger decides how much of it lands on disk: by default only its size, with
// debug.content its redacted, truncated text.
type Text struct {
	s string
	n int
}

// Content wraps s for Event, to be cut at n runes when content is logged.
func Content(s string, n int) Text { return Text{s: s, n: n} }

// render decides how much of user text reaches the log. Without content mode
// only its length is kept: a digest of a short prompt could be confirmed
// offline by hashing guesses.
func (l *Logger) render(t Text) any {
	if l.content {
		return Snippet(redactWindow(t.s, t.n), t.n)
	}
	return map[string]any{"len": utf8.RuneCountInString(t.s)}
}

// redactWindow redacts only the prefix that can survive truncation to n
// runes (plus a margin, so a credential straddling the cut is still caught):
// tool output can be megabytes, and redaction is linear in the input.
func redactWindow(s string, n int) string {
	limit := n*4 + 512 // bytes: up to 4 per rune, plus the margin
	if len(s) > limit {
		s = s[:limit]
	}
	return redact.String(s)
}

// urlQuery matches the query and fragment of a URL: tokens, OAuth codes and
// signatures travel there and error messages repeat whole URLs.
var urlQuery = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://[^\s?#"'<>]+)[?#][^\s"'<>]*`)

// scrub prepares a plain string field. Error messages and other free-form
// strings repeat what the user sent, so both modes cut URL queries, redact
// credential shapes and cap the length; the diagnostic text itself stays.
func scrub(s string) string {
	s = redactWindow(s, maxPlainRunes)
	s = urlQuery.ReplaceAllString(s, "$1?[…]")
	return Snippet(s, maxPlainRunes)
}

// maxPlainRunes caps plain string fields.
const maxPlainRunes = 300

// Open returns a Logger configured from cfg, with environment overrides.
//
// Env overrides (highest precedence):
//   - ANCHORED_DEBUG=1|true|on  → force enable
//   - ANCHORED_DEBUG=0|false|off → force disable
//   - ANCHORED_DEBUG_PATH=/path  → override log file path
//
// When disabled, Open returns a zero-value Logger that no-ops on Event so
// call sites stay branch-free.
func Open(cfg *config.Config) *Logger {
	enabled, path := resolve(cfg)
	if !enabled || path == "" {
		return &Logger{}
	}

	st := InspectAt(cfg, path)
	forced := forcedByEnv()
	if st.Expired && !forced {
		return &Logger{}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		// Surface the failure once on stderr so the user isn't left wondering
		// why their explicitly-enabled log is silent. Hook stderr is captured
		// by Claude Code, so the message is reachable.
		fmt.Fprintf(os.Stderr, "anchored: debug log disabled (mkdir %s: %v)\n", filepath.Dir(path), err)
		return &Logger{}
	}

	if st.Since.IsZero() {
		// No marker: this session starts now. A log already there was written
		// before events were redacted, so it is set aside rather than rotated.
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			if err := os.Rename(path, path+legacySuffix); err == nil {
				_ = os.Chmod(path+legacySuffix, 0o600)
			}
		}
		_ = writeNoFollow(path+sinceSuffix, []byte(now().UTC().Format(time.RFC3339)))
		st = InspectAt(cfg, path)
	}

	rotate(path)
	f, err := openLog(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anchored: debug log disabled (open %s: %v)\n", path, err)
		return &Logger{}
	}
	l := &Logger{file: f, enabled: true, path: path, content: contentMode(cfg)}
	if !forced {
		l.expires = st.Expires
	}
	return l
}

// openLog opens the log for appending, owner-only. 0o600 because events
// embed prompt sizes, tool names and error text; fchmod fixes a file an older
// version created 0644, and the open refuses to follow a symlink planted at
// the path.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|noFollow, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	return f, nil
}

func writeNoFollow(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|noFollow, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// rotate moves a log that reached maxLogBytes to path.1, shifting older
// files up and dropping the one past keepRotated. Processes that still hold
// the old file notice the rename on their next rotation check and reopen.
func rotate(path string) {
	// Lstat: a symlink at the log path is never opened (openLog refuses it),
	// so it is not rotated either, and the chmod below cannot reach its target.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < maxLogBytes {
		return
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keepRotated))
	for i := keepRotated - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", path, i), fmt.Sprintf("%s.%d", path, i+1))
	}
	if os.Rename(path, path+".1") == nil {
		_ = os.Chmod(path+".1", 0o600)
	}
}

// maybeRotateLocked runs every rotationCheckEvery writes: it rotates a log
// that grew past the limit and reopens the path when another process rotated
// the file this logger still holds.
func (l *Logger) maybeRotateLocked() {
	l.writes++
	if l.writes%rotationCheckEvery != 0 {
		return
	}
	held, err := l.file.Stat()
	if err != nil {
		return
	}
	current, err := os.Stat(l.path)
	if err == nil && os.SameFile(held, current) && current.Size() < maxLogBytes {
		return
	}
	rotate(l.path)
	f, err := openLog(l.path)
	if err != nil {
		return
	}
	_ = l.file.Close()
	l.file = f
}

// ForcedByEnv reports whether ANCHORED_DEBUG explicitly turns logging on for
// this process, which also bypasses the max_age_days expiry.
func ForcedByEnv() bool { return forcedByEnv() }

func forcedByEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ANCHORED_DEBUG"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func contentMode(cfg *config.Config) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ANCHORED_DEBUG_CONTENT"))) {
	case "1", "true", "on", "yes":
		return true
	case "0", "false", "off", "no":
		return false
	}
	return cfg != nil && cfg.Debug.Content
}

// Status describes the debug log for `anchored doctor`.
type Status struct {
	Enabled bool
	Path    string
	Size    int64
	Since   time.Time
	Expires time.Time
	Expired bool
	Content bool
	// Legacy is set when a log from before redaction sits at path.legacy.
	Legacy bool
}

// Inspect reports the debug log's state without opening it for writing.
func Inspect(cfg *config.Config) Status {
	enabled, path := resolve(cfg)
	st := InspectAt(cfg, path)
	st.Enabled = enabled
	return st
}

// InspectAt reports on the debug log at path (Enabled is left false).
func InspectAt(cfg *config.Config, path string) Status {
	st := Status{Path: path, Content: contentMode(cfg)}
	if path == "" {
		return st
	}
	if info, err := os.Stat(path); err == nil {
		st.Size = info.Size()
	}
	if _, err := os.Stat(path + legacySuffix); err == nil {
		st.Legacy = true
	}
	raw, err := os.ReadFile(path + sinceSuffix)
	if err != nil {
		return st
	}
	since, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return st
	}
	st.Since = since
	if cfg != nil && cfg.Debug.MaxAgeDays > 0 {
		st.Expires = since.Add(time.Duration(cfg.Debug.MaxAgeDays) * 24 * time.Hour)
		st.Expired = now().After(st.Expires)
	}
	return st
}

// resolve returns (enabled, path) honoring env > config > defaults.
func resolve(cfg *config.Config) (bool, string) {
	enabled := false
	path := ""
	if cfg != nil {
		enabled = cfg.Debug.Enabled
		path = cfg.Debug.Path
	}

	if v, ok := os.LookupEnv("ANCHORED_DEBUG"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			enabled = true
		case "0", "false", "off", "no", "":
			enabled = false
		}
	}

	if v := strings.TrimSpace(os.Getenv("ANCHORED_DEBUG_PATH")); v != "" {
		path = v
	}

	if enabled && path == "" {
		// Default: a per-user file inside ~/.anchored so debug data lives
		// alongside the rest of anchored's state.
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, ".anchored", "debug.log")
		}
	}

	return enabled, expandHome(path)
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// Path returns the resolved log path, or "" when disabled.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Enabled reports whether this logger will actually write.
func (l *Logger) Enabled() bool {
	return l != nil && l.enabled && l.file != nil
}

// Event appends an NDJSON line. fields are merged with built-in {ts, event}
// keys; collisions are won by built-ins to keep the schema predictable.
func (l *Logger) Event(name string, fields map[string]any) {
	if !l.Enabled() {
		return
	}

	rec := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		switch val := v.(type) {
		case Text:
			rec[k] = l.render(val)
		case string:
			rec[k] = scrub(val)
		case error:
			// An error value would marshal as {} and lose the diagnostic.
			rec[k] = scrub(val.Error())
		default:
			rec[k] = v
		}
	}
	rec["ts"] = now().UTC().Format(time.RFC3339Nano)
	rec["event"] = name
	rec["pid"] = os.Getpid()

	line, err := json.Marshal(rec)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || l.file == nil {
		return
	}
	if !l.expires.IsZero() && now().After(l.expires) {
		_ = l.file.Close()
		l.file = nil
		l.enabled = false
		return
	}
	_, _ = l.file.Write(append(line, '\n'))
	l.maybeRotateLocked()
}

// Close flushes and releases the underlying file handle. Safe on nil.
func (l *Logger) Close() error {
	if !l.Enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	l.file = nil
	l.enabled = false
	return err
}

// Snippet truncates s to n runes, appending an ellipsis when trimmed. Use
// this on user-facing strings (prompt bodies, tool args) so the log doesn't
// balloon with multi-KB payloads.
//
// Rune-aware (not byte-aware) on purpose: PT-BR / multilingual prompts
// would otherwise be cut mid-codepoint, producing invalid UTF-8 that
// json.Marshal silently replaces with U+FFFD — corrupting the very
// evidence the debug log exists to capture.
func Snippet(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	out := make([]rune, 0, n)
	for _, r := range s {
		out = append(out, r)
		if len(out) == n {
			break
		}
	}
	return string(out) + "…"
}
