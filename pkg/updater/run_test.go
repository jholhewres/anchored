package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	// Scoped to the log arm on purpose: Check only ever produces the reasons
	// it defines, so an unrecognized one cannot be reached through Run without
	// inventing a guard. What is testable here is that the default arm treats
	// it as a refusal and says so at WARN. That Run stops on every reason it
	// CAN produce is covered by TestRun_NoKnownRefusalReachesTheDownload,
	// which counts requests against a real server.
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

// Every reason logBlockedUpdate knows about produces a line, except the two
// that are silent by design. This covers the log arm only; whether Run stops
// is asserted separately, against a server that counts what it was asked for.
func TestLogBlockedUpdate_EveryReasonIsAccountedFor(t *testing.T) {
	for _, tc := range []struct {
		reason     BlockReason
		wantSilent bool
	}{
		{BlockDevBuild, false},
		{BlockOutsideCanonical, false},
		{BlockNotNewer, false},
		{BlockReason("some-future-guard"), false},
		// The user asked for no updates; saying so on every startup is noise.
		{BlockEnvDisabled, true},
		{BlockNoVersion, true},
	} {
		h := &capturingHandler{}
		logBlockedUpdate(slog.New(h), Result{Blocked: tc.reason, Current: "0.17.0", Latest: "0.18.0"})
		if got := len(h.records) == 0; got != tc.wantSilent {
			t.Errorf("%q: silent=%v, want %v (%s)", tc.reason, got, tc.wantSilent, h.messages())
		}
	}
}

// The allowlist itself, driven through Run and asserted by counting requests:
// no reason Check can produce may reach the download. Deleting the
// `if res.Blocked != BlockNone { logBlockedUpdate; return }` block in Run
// makes this fail — BlockNotNewer would fall through and fetch the asset.
func TestRun_NoKnownRefusalReachesTheDownload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		canon   bool
		env     map[string]string
		// wantReleaseLookup is true only where the refusal is decided after
		// the release is resolved; a local guard must not touch the network.
		wantReleaseLookup bool
	}{
		{name: "dev build", version: "0.17.0-dev+gabc", canon: true},
		{name: "no version", version: "", canon: true},
		{name: "env kill switch", version: "0.17.0", canon: true, env: map[string]string{"ANCHORED_NO_AUTOUPDATE": "1"}},
		{name: "outside canonical dir", version: "0.17.0", canon: false},
		{name: "not newer", version: "0.18.0", canon: true, wantReleaseLookup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var releaseHits, assetHits atomic.Int32
			countingRelease(t, "0.18.0", &releaseHits, &assetHits)

			binPath := filepath.Join(t.TempDir(), "anchored")
			if tc.canon {
				binPath = canonicalBin(t)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			h := &capturingHandler{}
			Run(context.Background(), Options{
				CurrentVersion: tc.version,
				BinPath:        binPath,
				Logger:         slog.New(h),
			})

			if n := assetHits.Load(); n != 0 {
				t.Errorf("a refused update downloaded the asset %d time(s):\n%s", n, h.messages())
			}
			if got := releaseHits.Load() > 0; got != tc.wantReleaseLookup {
				t.Errorf("release lookup happened=%v, want %v:\n%s", got, tc.wantReleaseLookup, h.messages())
			}
			if _, err := os.Stat(binPath + ".prev"); err == nil {
				t.Error("a refused update wrote a backup, so it reached the swap")
			}
		})
	}
}

// countingRelease is fakeRelease with counters on the two routes that matter:
// the release lookup and the asset download. It serves a real tarball so a
// fall-through would get all the way to the swap rather than erroring early
// and looking like the refusal held.
func countingRelease(t *testing.T, version string, releaseHits, assetHits *atomic.Int32) {
	t.Helper()
	assetName := fmt.Sprintf("anchored_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	body, sum := makeTarGz(t, []byte("REPLACEMENT-BINARY"))

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		assetHits.Add(1)
		if _, err := w.Write(body); err != nil {
			t.Errorf("write asset: %v", err)
		}
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "%s  %s\n", sum, assetName); err != nil {
			t.Errorf("write checksums: %v", err)
		}
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		releaseHits.Add(1)
		payload := map[string]any{
			"tag_name": "v" + version,
			"assets": []map[string]string{
				{"name": assetName, "browser_download_url": srv.URL + "/" + assetName},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/checksums.txt"},
			},
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode release: %v", err)
		}
	})

	orig := releaseAPIURL
	releaseAPIURL = srv.URL + "/release?repo=%s"
	t.Cleanup(func() { releaseAPIURL = orig })
}
