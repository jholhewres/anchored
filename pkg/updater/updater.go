// Package updater performs background self-update from GitHub releases.
//
// On startup the MCP server invokes Run, which checks the latest release,
// downloads the matching tarball, and atomically replaces the running
// binary. The currently executing process keeps the old in-memory image
// (Linux holds the inode open via /proc/self/exe), so the swap is safe;
// the next invocation picks up the new binary.
package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRepo  = "jholhewres/anchored"
	apiURL       = "https://api.github.com/repos/%s/releases/latest"
	checkTimeout = 8 * time.Second
	dlTimeout    = 90 * time.Second
)

// Options controls a single update attempt.
type Options struct {
	Repo           string // GitHub owner/repo. Empty falls back to defaultRepo.
	CurrentVersion string // Semver without leading "v".
	BinPath        string // Path to the binary to replace. Empty resolves via os.Executable.
	Logger         *slog.Logger
}

// Run performs a non-blocking self-update. It is safe to invoke from a
// goroutine: any failure is logged and swallowed.
func Run(ctx context.Context, opts Options) {
	if os.Getenv("ANCHORED_NO_AUTOUPDATE") == "1" {
		return
	}

	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	if opts.CurrentVersion == "" {
		return
	}

	if IsDevBuild(opts.CurrentVersion) {
		// A dev build installed into the canonical dir is an intentional
		// local checkout (make sync-bin); self-updating it would silently
		// revert the developer's working binary back to the release tag.
		log.Debug("autoupdate: dev build, self-update disabled", "current", opts.CurrentVersion)
		return
	}

	binPath := opts.BinPath
	if binPath == "" {
		exe, err := os.Executable()
		if err != nil {
			log.Debug("autoupdate: cannot resolve executable", "error", err)
			return
		}
		binPath, _ = filepath.EvalSymlinks(exe)
		if binPath == "" {
			binPath = exe
		}
	}

	// Only auto-update binaries installed under ~/.anchored/bin. Dev builds
	// (running ./bin/anchored from a checkout) must never be overwritten.
	home, _ := os.UserHomeDir()
	canonical := filepath.Join(home, ".anchored", "bin")
	if !strings.HasPrefix(binPath, canonical+string(filepath.Separator)) {
		log.Debug("autoupdate: skip, binary outside canonical dir", "path", binPath)
		return
	}

	repo := opts.Repo
	if repo == "" {
		repo = defaultRepo
	}

	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	latest, asset, assetName, checksums, err := fetchLatest(checkCtx, repo)
	if err != nil {
		// Promoted from Debug → Warn: silent failures here meant users had
		// no signal when their auto-update was broken (network down, repo
		// renamed, asset matrix changed, GitHub rate-limit). Background
		// goroutine, so noise stays minimal — one line per startup at most.
		log.Warn("autoupdate: check failed", "error", err)
		return
	}

	if !isNewer(latest, opts.CurrentVersion) {
		log.Debug("autoupdate: already on latest", "current", opts.CurrentVersion, "latest", latest)
		return
	}

	log.Info("autoupdate: new version available", "current", opts.CurrentVersion, "latest", latest)

	dlCtx, dlCancel := context.WithTimeout(ctx, dlTimeout)
	defer dlCancel()

	wantSum, err := fetchChecksum(dlCtx, checksums, assetName)
	if err != nil {
		// Refuse to install without a verified digest. GoReleaser publishes
		// checksums.txt for every release; if it isn't reachable, treat the
		// download as untrusted and skip the swap.
		log.Warn("autoupdate: checksum lookup failed, skipping install", "error", err)
		return
	}

	if err := downloadAndReplace(dlCtx, asset, binPath, wantSum); err != nil {
		log.Warn("autoupdate: install failed", "error", err)
		return
	}

	log.Info("autoupdate: installed, restart MCP server to activate", "version", latest, "path", binPath, "backup", binPath+".prev")
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatest returns version, asset URL, asset filename, and the
// checksums.txt URL from the latest release. The asset filename is needed
// later to look up the right line in checksums.txt; the checksum URL is
// resolved here so we can fail fast if GoReleaser stopped publishing it.
func fetchLatest(ctx context.Context, repo string) (version string, assetURL string, assetName string, checksumsURL string, err error) {
	url := fmt.Sprintf(apiURL, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "anchored-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", "", "", fmt.Errorf("decode release: %w", err)
	}

	version = strings.TrimPrefix(rel.TagName, "v")
	if version == "" {
		return "", "", "", "", errors.New("empty tag_name in release")
	}

	wantSuffix := fmt.Sprintf("_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, wantSuffix) {
			assetURL = a.BrowserDownloadURL
			assetName = a.Name
			break
		}
	}
	if assetURL == "" {
		return "", "", "", "", fmt.Errorf("no asset for %s/%s with version %s", runtime.GOOS, runtime.GOARCH, version)
	}

	for _, a := range rel.Assets {
		if a.Name == "checksums.txt" {
			checksumsURL = a.BrowserDownloadURL
			break
		}
	}
	if checksumsURL == "" {
		return "", "", "", "", errors.New("checksums.txt not in release assets")
	}
	return version, assetURL, assetName, checksumsURL, nil
}

// fetchChecksum downloads checksums.txt and returns the lowercase hex sha256
// for assetName. The file is small (one line per asset, ~80 bytes each), so
// reading it whole is fine. Format follows GoReleaser/sha256sum convention:
//   <hex digest>  <filename>
func fetchChecksum(ctx context.Context, url, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "anchored-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksums.txt: HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		// Two-field split: digest and filename. GoReleaser uses two spaces;
		// allow any whitespace to stay compatible with sha256sum's output.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == assetName {
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read checksums: %w", err)
	}
	return "", fmt.Errorf("checksum not found for %s", assetName)
}

// downloadAndReplace streams the tarball, validates its SHA-256 against
// wantSum, extracts the embedded `anchored` binary, and atomically swaps
// it into dst while keeping the previous binary at dst+".prev" so a bad
// update can be rolled back manually with one rename.
func downloadAndReplace(ctx context.Context, url, dst, wantSum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "anchored-updater")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	// Tee the raw response body into the hasher so we digest exactly what
	// the upstream signed. gzip.NewReader is allowed to stop reading at the
	// gzip EOF before consuming the whole HTTP body, so we drain the rest
	// after extraction completes — otherwise the digest would be partial.
	hasher := sha256.New()
	tee := io.TeeReader(resp.Body, hasher)

	gz, err := gzip.NewReader(tee)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	tmpPath := dst + ".new"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}

	written := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "anchored" {
			continue
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("write tmp: %w", err)
		}
		written = true
		break
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp: %w", err)
	}
	if !written {
		os.Remove(tmpPath)
		return errors.New("anchored binary not found in tarball")
	}

	// Drain any trailing bytes the gzip reader didn't consume so the hash
	// covers the full tar.gz payload, then compare.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("drain body: %w", err)
	}
	gotSum := hex.EncodeToString(hasher.Sum(nil))
	if gotSum != strings.ToLower(wantSum) {
		os.Remove(tmpPath)
		return fmt.Errorf("checksum mismatch: want %s got %s", wantSum, gotSum)
	}

	// Two-step swap so a bad new binary can be rolled back to .prev.
	// On Linux/macOS rename is atomic within the same filesystem, and dst
	// + ".new" + ".prev" all live in dst's parent dir by construction.
	prevPath := dst + ".prev"
	// Best-effort: only matters when dst already exists (fresh installs
	// from scratch hit ENOENT, which is fine).
	if _, statErr := os.Stat(dst); statErr == nil {
		_ = os.Remove(prevPath) // discard any stale backup from a prior cycle
		if err := os.Rename(dst, prevPath); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("backup current: %w", err)
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		// Recovery: try to put the old binary back so the user isn't left
		// with no executable at all.
		_ = os.Rename(prevPath, dst)
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// IsDevBuild reports whether v is a local development build: the bare "dev"
// placeholder (go build without ldflags) or a `make build` stamp carrying
// "-dev+g<hash>". Dev builds are excluded from self-update and from plugin
// drift comparison — the git suffix makes both comparisons meaningless.
func IsDevBuild(v string) bool {
	return v == "dev" || strings.Contains(v, "-dev+")
}

// isNewer reports whether latest > current using lexical semver split.
// Pre-release suffixes (-rc1) compare lexicographically; "1.2.3" beats "1.2.3-rc1".
func isNewer(latest, current string) bool {
	a := splitSemver(latest)
	b := splitSemver(current)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	// Equal numeric parts: stable (no suffix) > pre-release.
	la := preRelease(latest)
	lc := preRelease(current)
	if la == "" && lc != "" {
		return true
	}
	if la != "" && lc == "" {
		return false
	}
	return la > lc
}

func splitSemver(v string) [3]int {
	v = strings.SplitN(v, "-", 2)[0]
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}

func preRelease(v string) string {
	if i := strings.Index(v, "-"); i >= 0 {
		return v[i+1:]
	}
	return ""
}
