package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

func probeEntry(url, key string) config.RemoteEntry {
	return config.RemoteEntry{Name: "test", ServerURL: url, APIKey: key}
}

func TestProbeRemote_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"service":"anchored-oss","version":"0.5.0","status":"ok"}`))
		case "/v1/me":
			if r.Header.Get("Authorization") != "Bearer good-key" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := probeRemote(context.Background(), probeEntry(srv.URL, "good-key"))
	if p.Class != "ok" {
		t.Fatalf("class = %q, want ok", p.Class)
	}
	if p.Version != "0.5.0" {
		t.Fatalf("version = %q, want 0.5.0", p.Version)
	}
	if p.Latency <= 0 {
		t.Fatalf("latency not measured: %v", p.Latency)
	}
}

func TestProbeRemote_AuthRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.Write([]byte(`{"version":"0.5.0"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := probeRemote(context.Background(), probeEntry(srv.URL, "bad-key"))
	if p.Class != "auth" {
		t.Fatalf("class = %q, want auth", p.Class)
	}
}

func TestProbeRemote_Timeout(t *testing.T) {
	old := probeTimeout
	probeTimeout = 150 * time.Millisecond
	defer func() { probeTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}))
	defer srv.Close()

	p := probeRemote(context.Background(), probeEntry(srv.URL, "k"))
	if p.Class != "timeout" {
		t.Fatalf("class = %q, want timeout", p.Class)
	}
}

func TestProbeRemote_TLSUntrusted(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// Default client does not trust httptest's self-signed CA.
	p := probeRemote(context.Background(), probeEntry(srv.URL, "k"))
	if p.Class != "tls" {
		t.Fatalf("class = %q, want tls", p.Class)
	}
}

func TestProbeRemote_DNSFailure(t *testing.T) {
	p := probeRemote(context.Background(), probeEntry("http://nonexistent-host-zz.invalid", "k"))
	// Some resolvers wrap NXDOMAIN differently; dns is expected, unreachable
	// acceptable only if the platform resolver hides the DNS error type.
	if p.Class != "dns" {
		t.Fatalf("class = %q, want dns", p.Class)
	}
}

func TestKeyPrefix_NeverFullKey(t *testing.T) {
	full := "anc_live_0123456789abcdef0123456789abcdef"
	got := keyPrefix(full)
	if strings.Contains(got, full[8:16]) {
		t.Fatalf("prefix leaked key body: %q", got)
	}
	if len([]rune(got)) > 9 { // 8 chars + ellipsis
		t.Fatalf("prefix too long: %q", got)
	}
	// Short keys are fully masked — never printed verbatim.
	if got := keyPrefix("short"); got != "*****" {
		t.Fatalf("short key not masked: %q", got)
	}
}

func TestSanitizeURL_StripsUserinfo(t *testing.T) {
	got := sanitizeURL("https://user:secretpass@host.example.com:8443/base")
	if strings.Contains(got, "secretpass") || strings.Contains(got, "user") {
		t.Fatalf("userinfo leaked: %q", got)
	}
	if !strings.Contains(got, "host.example.com:8443") {
		t.Fatalf("host lost: %q", got)
	}
	if sanitizeURL("https://plain.example.com") != "https://plain.example.com" {
		t.Fatal("plain URL must pass through")
	}
}

func TestDoctorJSONShape(t *testing.T) {
	doctorChecks = nil
	doctorJSONMode = true // suppress prints; exercise the REAL collector
	defer func() { doctorChecks = nil; doctorJSONMode = false }()

	recordCheck("ok", "binary", "v0.6.14", "", false)
	recordCheck("failed", "remote \"team\"", "connectivity failed: dns", "check DNS", true)
	recordCheck("skipped", "project identity", "offline", "", false)

	out := struct {
		Version string        `json:"version"`
		Checks  []checkResult `json:"checks"`
	}{Version: "test", Checks: doctorChecks}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed struct {
		Version string `json:"version"`
		Checks  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Detail     string `json:"detail"`
			FixCommand string `json:"fix_command"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Checks) != 3 {
		t.Fatalf("checks = %d, want 3", len(parsed.Checks))
	}
	if parsed.Checks[1].Status != "failed" || parsed.Checks[1].FixCommand != "check DNS" {
		t.Fatalf("failed check shape: %+v", parsed.Checks[1])
	}
	if parsed.Checks[2].Status != "skipped" {
		t.Fatalf("skipped check shape: %+v", parsed.Checks[2])
	}
}

func TestRemoteConfigSanity_DefaultDetection(t *testing.T) {
	doctorChecks = nil
	doctorJSONMode = true // suppress prints
	defer func() { doctorChecks = nil; doctorJSONMode = false }()

	cfg := &config.Config{Remotes: map[string]config.RemoteEntry{
		"a": {Name: "a", ServerURL: "https://a.example.com", APIKey: "k"},
		"b": {Name: "b", ServerURL: "https://b.example.com", APIKey: "k"},
	}}
	checkRemoteConfigSanity(cfg)

	var noDefault bool
	for _, c := range doctorChecks {
		if c.Name == "default remote" && c.Status == "failed" &&
			strings.Contains(c.Detail, "no remote has default") {
			noDefault = true
		}
	}
	if !noDefault {
		t.Fatalf("missing no-default finding: %+v", doctorChecks)
	}
}

// --- test helpers ---

// withDoctorChecks resets the package-level collector into JSON mode (so
// nothing is printed to stdout during tests) and restores it on cleanup.
func withDoctorChecks(t *testing.T) {
	t.Helper()
	doctorChecks = nil
	doctorJSONMode = true
	t.Cleanup(func() { doctorChecks = nil; doctorJSONMode = false })
}

func findCheck(name string) *checkResult {
	for i := range doctorChecks {
		if doctorChecks[i].Name == name {
			return &doctorChecks[i]
		}
	}
	return nil
}

func writeDoctorFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- version normalization (S3.2) ---

func TestFormatV_SingleLeadingV(t *testing.T) {
	for _, in := range []string{"v0.10.0", "0.10.0"} {
		if got := formatV(in); got != "v0.10.0" {
			t.Errorf("formatV(%q) = %q, want v0.10.0", in, got)
		}
	}
}

// --- Cursor activation (S1.6) ---

func TestCheckCursorActivation_MissingCursorDirSkipsSilently(t *testing.T) {
	withDoctorChecks(t)
	home := t.TempDir() // no .cursor subdir
	checkCursorActivation(home)
	if len(doctorChecks) != 0 {
		t.Fatalf("expected no checks when ~/.cursor is absent, got %+v", doctorChecks)
	}
}

func TestCheckCursorHooks_MissingEventsFails(t *testing.T) {
	withDoctorChecks(t)
	hooksPath := filepath.Join(t.TempDir(), "hooks.json")
	writeDoctorFixture(t, hooksPath, `{"version":1,"hooks":{
		"beforeShellExecution":[{"command":"/opt/anchored/bin/anchored hook pretooluse"}]
	}}`)

	checkCursorHooks(hooksPath)

	c := findCheck("Cursor hooks.json wires anchored hooks")
	if c == nil || c.Status != "failed" {
		t.Fatalf("expected failed check for missing events, got %+v", doctorChecks)
	}
	if !strings.Contains(c.Detail, "afterFileEdit") || !strings.Contains(c.Detail, "stop") {
		t.Fatalf("expected missing-events detail, got %+v", c)
	}
}

func TestCheckCursorHooks_BareCommandWarns(t *testing.T) {
	withDoctorChecks(t)
	hooksPath := filepath.Join(t.TempDir(), "hooks.json")
	writeDoctorFixture(t, hooksPath, `{"version":1,"hooks":{
		"beforeShellExecution":[{"command":"anchored hook pretooluse"}],
		"afterFileEdit":[{"command":"anchored hook posttooluse"}],
		"stop":[{"command":"anchored hook stop"}]
	}}`)

	checkCursorHooks(hooksPath)

	if c := findCheck("Cursor hooks.json wires beforeShellExecution/afterFileEdit/stop"); c == nil || c.Status != "ok" {
		t.Fatalf("expected events check to pass, got %+v", doctorChecks)
	}
	c := findCheck("Cursor hook commands use an absolute path")
	if c == nil || c.Status != "warn" {
		t.Fatalf("expected warn for bare (PATH-dependent) command, got %+v", doctorChecks)
	}
}

func TestCheckCursorHooks_CompletePasses(t *testing.T) {
	withDoctorChecks(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "anchored")
	writeDoctorFixture(t, bin, "#!/bin/sh\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}

	hooksPath := filepath.Join(dir, "hooks.json")
	writeDoctorFixture(t, hooksPath, fmt.Sprintf(`{"version":1,"hooks":{
		"beforeShellExecution":[{"command":%q}],
		"afterFileEdit":[{"command":%q}],
		"stop":[{"command":%q}]
	}}`, bin+" hook pretooluse", bin+" hook posttooluse", bin+" hook stop"))

	checkCursorHooks(hooksPath)

	for _, name := range []string{
		"Cursor hooks.json wires beforeShellExecution/afterFileEdit/stop",
		"Cursor hook commands use an absolute path",
	} {
		if c := findCheck(name); c == nil || c.Status != "ok" {
			t.Fatalf("expected %q to pass, got %+v", name, doctorChecks)
		}
	}
}

func TestCheckCursorRule(t *testing.T) {
	withDoctorChecks(t)
	rulePath := filepath.Join(t.TempDir(), "rules", "anchored.mdc")

	checkCursorRule(rulePath)
	if c := findCheck("Cursor rule ~/.cursor/rules/anchored.mdc present"); c == nil || c.Status != "failed" {
		t.Fatalf("expected failed for missing rule, got %+v", doctorChecks)
	}

	doctorChecks = nil
	writeDoctorFixture(t, rulePath, "content")
	checkCursorRule(rulePath)
	if c := findCheck("Cursor rule ~/.cursor/rules/anchored.mdc present"); c == nil || c.Status != "ok" {
		t.Fatalf("expected ok when rule present, got %+v", doctorChecks)
	}
}

func TestCheckCursorActivation_CompleteFixture(t *testing.T) {
	withDoctorChecks(t)
	home := t.TempDir()
	cursorDir := filepath.Join(home, ".cursor")
	bin := filepath.Join(home, "bin", "anchored")
	writeDoctorFixture(t, bin, "#!/bin/sh\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}

	writeDoctorFixture(t, filepath.Join(cursorDir, "hooks.json"), fmt.Sprintf(`{"version":1,"hooks":{
		"beforeShellExecution":[{"command":%q}],
		"afterFileEdit":[{"command":%q}],
		"stop":[{"command":%q}]
	}}`, bin+" hook pretooluse", bin+" hook posttooluse", bin+" hook stop"))
	writeDoctorFixture(t, filepath.Join(cursorDir, "rules", "anchored.mdc"), "rule")

	checkCursorActivation(home)

	for _, name := range []string{
		"Cursor hooks.json wires beforeShellExecution/afterFileEdit/stop",
		"Cursor hook commands use an absolute path",
		"Cursor rule ~/.cursor/rules/anchored.mdc present",
	} {
		if c := findCheck(name); c == nil || c.Status != "ok" {
			t.Fatalf("expected %q ok, got %+v", name, doctorChecks)
		}
	}
}

// --- Embedding coverage threshold (S2.4) ---

func TestEmbeddingCoverage_Boundaries(t *testing.T) {
	cases := []struct {
		embedded, total int
		wantOK          bool
	}{
		{0, 0, true},     // empty corpus: nothing to embed, treated as covered
		{79, 100, false}, // just below threshold
		{80, 100, true},  // exactly at threshold
		{100, 100, true},
	}
	for _, c := range cases {
		if got := embeddingCoverageOK(c.embedded, c.total); got != c.wantOK {
			t.Errorf("embeddingCoverageOK(%d,%d) = %v, want %v", c.embedded, c.total, got, c.wantOK)
		}
	}
}

// --- Debug log observability (S3.3) ---

func TestCheckDebugLog_DisabledIsOK(t *testing.T) {
	withDoctorChecks(t)
	os.Unsetenv("ANCHORED_DEBUG")
	os.Unsetenv("ANCHORED_DEBUG_PATH")

	cfg := &config.Config{Debug: config.DebugConfig{Enabled: false}}
	checkDebugLog(cfg, t.TempDir())

	if c := findCheck("hook observability"); c == nil || c.Status != "ok" {
		t.Fatalf("expected ok for disabled debug, got %+v", doctorChecks)
	}
}

func TestCheckDebugLog_StaleWarns(t *testing.T) {
	withDoctorChecks(t)
	os.Unsetenv("ANCHORED_DEBUG")
	os.Unsetenv("ANCHORED_DEBUG_PATH")

	home := t.TempDir()
	logPath := filepath.Join(home, ".anchored", "debug.log")
	writeDoctorFixture(t, logPath, "{}\n")
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Debug: config.DebugConfig{Enabled: true}}
	checkDebugLog(cfg, home)

	c := findCheck("hook observability")
	if c == nil || c.Status != "warn" {
		t.Fatalf("expected warn for stale debug log, got %+v", doctorChecks)
	}
}

func TestCheckDebugLog_FreshIsOK(t *testing.T) {
	withDoctorChecks(t)
	os.Unsetenv("ANCHORED_DEBUG")
	os.Unsetenv("ANCHORED_DEBUG_PATH")

	home := t.TempDir()
	writeDoctorFixture(t, filepath.Join(home, ".anchored", "debug.log"), "{}\n")

	cfg := &config.Config{Debug: config.DebugConfig{Enabled: true}}
	checkDebugLog(cfg, home)

	if c := findCheck("hook observability"); c == nil || c.Status != "ok" {
		t.Fatalf("expected ok for fresh debug log, got %+v", doctorChecks)
	}
}

// --- Model label hygiene ---

func TestCheckModelLabel_MismatchWarns(t *testing.T) {
	withDoctorChecks(t)
	dir := t.TempDir()
	writeDoctorFixture(t, filepath.Join(dir, "paraphrase-multilingual-MiniLM-L12-v2", "model.onnx"), "x")

	checkModelLabel("all-MiniLM-L6-v2", dir)

	c := findCheck("embedding.model label matches model_dir")
	if c == nil || c.Status != "warn" {
		t.Fatalf("expected warn for label mismatch, got %+v", doctorChecks)
	}
}

func TestCheckModelLabel_MatchIsOK(t *testing.T) {
	withDoctorChecks(t)
	dir := t.TempDir()
	writeDoctorFixture(t, filepath.Join(dir, "all-MiniLM-L6-v2", "model.onnx"), "x")

	checkModelLabel("all-MiniLM-L6-v2", dir)

	c := findCheck("embedding.model label matches model_dir (all-MiniLM-L6-v2)")
	if c == nil || c.Status != "ok" {
		t.Fatalf("expected ok for matching label, got %+v", doctorChecks)
	}
}

// --- Maintenance timer (S2.4) ---

func TestCheckMaintenanceTimerAt_NotInstalledWarns(t *testing.T) {
	withDoctorChecks(t)
	checkMaintenanceTimerAt(t.TempDir())

	c := findCheck("anchored-maintenance systemd timer")
	if c == nil || c.Status != "warn" {
		t.Fatalf("expected warn when timer unit is not installed, got %+v", doctorChecks)
	}
}
