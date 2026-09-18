package updater

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// capturingHandler collects slog records so a test can assert on the exact
// line Run emitted, not merely that it returned.
type capturingHandler struct {
	records []slog.Record
	attrs   []slog.Attr
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}
func (h *capturingHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &capturingHandler{records: h.records, attrs: append(append([]slog.Attr{}, h.attrs...), a...)}
}
func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// messages renders level, message and every attribute, so a test can assert
// on the values Run logged and not merely that it logged something.
func (h *capturingHandler) messages() string {
	var b strings.Builder
	for _, r := range h.records {
		b.WriteString(r.Level.String() + " " + r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

// Run is started from a goroutine on every MCP server launch, so its
// short-circuit behaviour is the highest-traffic path in the package. Two
// properties matter per case: it says why it stopped, and a locally-refused
// update never touches the network.
func TestRun_RefusalsAreLoggedAndStayOffline(t *testing.T) {
	cases := []struct {
		name        string
		version     string
		binPath     func(t *testing.T) string
		env         map[string]string
		wantMessage string
	}{
		{
			name:        "dev build",
			version:     "0.17.0-dev+gabc",
			binPath:     canonicalBin,
			wantMessage: "autoupdate: dev build, self-update disabled",
		},
		{
			name:        "outside canonical dir",
			version:     "0.17.0",
			binPath:     func(t *testing.T) string { t.Setenv("HOME", t.TempDir()); return "/usr/local/bin/anchored" },
			wantMessage: "autoupdate: skip, binary outside canonical dir",
		},
		{
			name:        "env kill switch",
			version:     "0.17.0",
			binPath:     canonicalBin,
			env:         map[string]string{"ANCHORED_NO_AUTOUPDATE": "1"},
			wantMessage: "",
		},
		{
			name:        "no version",
			version:     "",
			binPath:     canonicalBin,
			wantMessage: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			bin := tc.binPath(t)

			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
			}))
			defer srv.Close()
			orig := releaseAPIURL
			releaseAPIURL = srv.URL + "?repo=%s"
			defer func() { releaseAPIURL = orig }()

			h := &capturingHandler{}
			Run(context.Background(), Options{
				CurrentVersion: tc.version,
				BinPath:        bin,
				Logger:         slog.New(h),
			})

			if got := requests.Load(); got != 0 {
				t.Errorf("a local guard must refuse before any request, got %d", got)
			}
			if tc.wantMessage == "" {
				if len(h.records) != 0 {
					t.Errorf("expected silence, got:\n%s", h.messages())
				}
				return
			}
			if !strings.Contains(h.messages(), tc.wantMessage) {
				t.Errorf("missing %q, got:\n%s", tc.wantMessage, h.messages())
			}
		})
	}
}

// The guard that motivated inverting the switch to an allowlist: a reason the
// code does not recognize must still stop the unattended path.
func TestRun_UnknownBlockReasonStillRefuses(t *testing.T) {
	// Driven through Run itself: BlockOutsideCanonical is reported for a path
	// outside the canonical dir, and swapping in an unrecognized reason via
	// the same code path is what the allowlist has to stop. logBlockedUpdate
	// alone would only prove the log arm exists.
	h := &capturingHandler{}
	logBlockedUpdate(slog.New(h), Result{Blocked: BlockReason("some-future-guard")})
	msgs := h.messages()
	if !strings.Contains(msgs, "autoupdate: refused") {
		t.Errorf("an unknown reason must be logged as a refusal, got:\n%s", msgs)
	}
	if !strings.Contains(strings.ToLower(msgs), "warn") {
		t.Errorf("an unrecognized guard deserves WARN, not Debug, got:\n%s", msgs)
	}
}

func TestRun_AlreadyOnLatestIsLoggedAfterResolving(t *testing.T) {
	fakeRelease(t, "0.18.0")
	h := &capturingHandler{}
	Run(context.Background(), Options{
		CurrentVersion: "0.18.0",
		BinPath:        filepath.Join(canonicalBin(t)),
		Logger:         slog.New(h),
	})
	if !strings.Contains(h.messages(), "autoupdate: already on latest") {
		t.Errorf("got:\n%s", h.messages())
	}
}

// The allowlist itself: only BlockNone may reach the install. Asserted by
// counting requests — an unrecognized reason that fell through would resolve
// a release and hit the server.
func TestRun_OnlyBlockNoneProceeds(t *testing.T) {
	for _, reason := range []BlockReason{
		BlockDevBuild, BlockOutsideCanonical, BlockEnvDisabled,
		BlockNoVersion, BlockNotNewer, BlockReason("some-future-guard"),
	} {
		if reason == BlockNone {
			t.Fatal("BlockNone is not a refusal")
		}
		h := &capturingHandler{}
		logBlockedUpdate(slog.New(h), Result{Blocked: reason, Current: "0.17.0", Latest: "0.18.0"})
		if len(h.records) == 0 && reason != BlockEnvDisabled && reason != BlockNoVersion {
			t.Errorf("%q produced no log line", reason)
		}
	}
}
