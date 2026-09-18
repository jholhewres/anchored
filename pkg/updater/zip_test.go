package updater

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip produces a zip holding one entry with the given name, returning the
// bytes and their sha256 — the windows release ships zip, not tar.gz.
func makeZip(t *testing.T, entry string, payload []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func serveBytes(t *testing.T, name string, body []byte) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write(body); err != nil {
			t.Errorf("write body: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/" + name
}

// The windows archive is a zip containing anchored.exe. Before this, the
// extractor was tar-only and the name match was "anchored" exactly, so
// self-update could not complete on Windows at all.
func TestDownloadAndReplace_InstallsFromAWindowsZip(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	body, sum := makeZip(t, "anchored.exe", []byte("NEW-WINDOWS-BINARY"))
	url := serveBytes(t, "anchored_1.0.0_windows_amd64.zip", body)

	if err := downloadAndReplace(context.Background(), url, dst, sum); err != nil {
		t.Fatalf("downloadAndReplace: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW-WINDOWS-BINARY" {
		t.Fatalf("dst = %q", got)
	}
	if prev, err := os.ReadFile(dst + ".prev"); err != nil || string(prev) != "OLD" {
		t.Fatalf(".prev = %q, err %v", prev, err)
	}
}

func TestDownloadAndReplace_ZipWithBadChecksumLeavesTheBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	body, _ := makeZip(t, "anchored.exe", []byte("TAMPERED"))
	url := serveBytes(t, "anchored_1.0.0_windows_amd64.zip", body)

	err := downloadAndReplace(context.Background(), url, dst, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected a checksum mismatch, got %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "OLD" {
		t.Errorf("the installed binary was replaced: %q", got)
	}
	if _, err := os.Stat(dst + ".prev"); err == nil {
		t.Error(".prev written despite a rejected payload")
	}
}

func TestDownloadAndReplace_ZipWithoutTheBinaryIsRejected(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	body, sum := makeZip(t, "README.txt", []byte("nope"))
	url := serveBytes(t, "anchored_1.0.0_windows_amd64.zip", body)

	err := downloadAndReplace(context.Background(), url, dst, sum)
	if err == nil || !strings.Contains(err.Error(), "not found in zip") {
		t.Fatalf("expected the binary to be reported missing, got %v", err)
	}
	leaked, _ := filepath.Glob(filepath.Join(dir, ".anchored-new-*"))
	if len(leaked) != 0 {
		t.Errorf("staging file leaked: %v", leaked)
	}
}

// localSeam points the release API seam at http so assertTrustedDownloadURL
// stands down for a local test server, the same way the other tests do it.
func localSeam(t *testing.T) {
	t.Helper()
	orig := releaseAPIURL
	releaseAPIURL = "http://127.0.0.1/%s"
	t.Cleanup(func() { releaseAPIURL = orig })
}

// The darwin archives are built by a separate macOS job and carry a
// `<asset>.sha256` sidecar instead of appearing in checksums.txt. Without the
// fallback, every Mac resolved an asset and then refused to install it.
func TestResolveChecksum_FallsBackToTheSidecar(t *testing.T) {
	localSeam(t)
	asset := "anchored_1.0.0_darwin_arm64.tar.gz"
	digest := strings.Repeat("a", 64)

	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		// Covers only the GoReleaser-built archives, like the real one.
		if _, err := fmt.Fprintf(w, "%s  anchored_1.0.0_linux_amd64.tar.gz\n", strings.Repeat("b", 64)); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	mux.HandleFunc("/"+asset+".sha256", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "%s  %s\n", digest, asset); err != nil {
			t.Errorf("write: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := resolveChecksum(context.Background(), srv.URL+"/checksums.txt", srv.URL+"/"+asset, asset)
	if err != nil {
		t.Fatalf("sidecar fallback failed: %v", err)
	}
	if got != digest {
		t.Fatalf("got %q, want %q", got, digest)
	}
}

func TestResolveChecksum_ReportsBothFailures(t *testing.T) {
	localSeam(t)
	asset := "anchored_1.0.0_darwin_arm64.tar.gz"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := resolveChecksum(context.Background(), srv.URL+"/checksums.txt", srv.URL+"/"+asset, asset)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "sidecar") {
		t.Errorf("error should mention the sidecar attempt, got %v", err)
	}
}

func TestIsAnchoredBinary(t *testing.T) {
	for _, ok := range []string{"anchored", "anchored.exe", "./anchored", "dir/anchored.exe"} {
		if !isAnchoredBinary(ok) {
			t.Errorf("%q should match", ok)
		}
	}
	for _, bad := range []string{"anchored.txt", "anchoredx", "README", "anchored.exe.bak"} {
		if isAnchoredBinary(bad) {
			t.Errorf("%q should not match", bad)
		}
	}
}
