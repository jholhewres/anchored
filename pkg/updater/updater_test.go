package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.3.2", "0.3.1", true},
		{"0.3.1", "0.3.1", false},
		{"0.3.0", "0.3.1", false},
		{"1.0.0", "0.9.9", true},
		{"0.10.0", "0.9.9", true},
		{"0.3.2", "0.3.2-rc1", true},
		{"0.3.2-rc2", "0.3.2-rc1", true},
		{"0.3.2-rc1", "0.3.2", false},
		// Documents WHY the Run guard on IsDevBuild is load-bearing: without
		// it, an equal-content release "outranks" a dev stamp, so autoupdate
		// would revert a freshly installed local build.
		{"0.3.2", "0.3.2-dev+gabc1234.dirty", true},
	}
	for _, tc := range cases {
		got := isNewer(tc.latest, tc.current)
		if got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestIsDevBuild(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false},
		{"v0.17.0", false},
		{"dev", true},
		{"v0.17.0-dev+g3926a8c", true},
		{"v0.17.0-dev+g3926a8c.dirty", true},
	}
	for _, tc := range cases {
		if got := IsDevBuild(tc.v); got != tc.want {
			t.Errorf("IsDevBuild(%q) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestSplitSemver(t *testing.T) {
	got := splitSemver("0.10.2-rc1")
	want := [3]int{0, 10, 2}
	if got != want {
		t.Errorf("splitSemver = %v, want %v", got, want)
	}
}

// makeTarGz produces a single-file tar.gz containing an "anchored" entry
// with the given payload, returning (bytes, sha256-hex). Used to fake the
// release tarball in tests without hitting the network.
func makeTarGz(t *testing.T, payload []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "anchored", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestFetchChecksum_ParsesGoReleaserFormat(t *testing.T) {
	body := strings.Join([]string{
		"abc123def  anchored_0.4.5_linux_amd64.tar.gz",
		"deadbeef00  anchored_0.4.5_darwin_arm64.tar.gz",
		"",
	}, "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	got, err := fetchChecksum(context.Background(), srv.URL, "anchored_0.4.5_linux_amd64.tar.gz")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "abc123def" {
		t.Fatalf("want abc123def, got %s", got)
	}
}

func TestFetchChecksum_AssetMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "abc  some_other_asset.tar.gz")
	}))
	defer srv.Close()

	_, err := fetchChecksum(context.Background(), srv.URL, "anchored_x.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "checksum not found") {
		t.Fatalf("expected 'checksum not found', got %v", err)
	}
}

func TestDownloadAndReplace_VerifiesChecksumAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	// Pre-existing binary stands in for a previous version that should be
	// preserved as .prev after the swap.
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball, sum := makeTarGz(t, []byte("NEW-BINARY"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	if err := downloadAndReplace(context.Background(), srv.URL, dst, sum); err != nil {
		t.Fatalf("downloadAndReplace: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "NEW-BINARY" {
		t.Fatalf("dst content = %q, want NEW-BINARY", got)
	}
	prev, err := os.ReadFile(dst + ".prev")
	if err != nil {
		t.Fatalf("expected .prev backup, got err %v", err)
	}
	if string(prev) != "OLD" {
		t.Fatalf(".prev content = %q, want OLD", prev)
	}
}

func TestDownloadAndReplace_RejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "anchored")
	if err := os.WriteFile(dst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}

	tarball, _ := makeTarGz(t, []byte("TAMPERED"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarball)
	}))
	defer srv.Close()

	wrongSum := strings.Repeat("0", 64)
	err := downloadAndReplace(context.Background(), srv.URL, dst, wrongSum)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}

	// Original binary must be untouched on rejection — neither replaced
	// nor backed up to .prev.
	got, _ := os.ReadFile(dst)
	if string(got) != "OLD" {
		t.Fatalf("dst was modified despite checksum failure: %q", got)
	}
	if _, err := os.Stat(dst + ".prev"); err == nil {
		t.Fatal(".prev should not exist when install was rejected")
	}
	if _, err := os.Stat(dst + ".new"); err == nil {
		t.Fatal(".new tmp file leaked")
	}
}
