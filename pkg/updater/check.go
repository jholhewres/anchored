package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BlockReason names why an update was refused. The background path only logs
// it and stops, but a caller acting on an explicit user request needs the
// cause as data — some refusals are legitimately overridable, and a silent
// no-op gives the user nothing to act on.
type BlockReason string

const (
	BlockNone             BlockReason = ""
	BlockNoVersion        BlockReason = "no-version"
	BlockEnvDisabled      BlockReason = "env-disabled"
	BlockDevBuild         BlockReason = "dev-build"
	BlockOutsideCanonical BlockReason = "outside-canonical-dir"
	BlockNotNewer         BlockReason = "not-newer"
)

// Result carries what Check learned: the versions in play, the binary that
// would be replaced, the resolved release assets, and whether policy allows
// the swap. Apply consumes it unchanged.
type Result struct {
	Current      string
	Latest       string
	BinPath      string
	AssetURL     string
	AssetName    string
	ChecksumsURL string
	Newer        bool
	Blocked      BlockReason
}

// ErrChecksumLookup marks a failure to obtain the expected digest, as opposed
// to a failure while installing a payload whose digest was known. The two are
// not interchangeable: a download we cannot verify is skipped, never
// installed, so callers must be able to tell them apart.
var ErrChecksumLookup = errors.New("checksum lookup failed")

// ErrResolveExecutable marks a failure to locate the running binary, which
// leaves nothing to replace.
var ErrResolveExecutable = errors.New("cannot resolve executable")

// Check applies update policy and, unless a local guard already refused,
// resolves the latest release. It performs no writes.
//
// Local guards (env kill switch, missing version, dev build, binary outside
// the canonical install dir) short-circuit before any network call, so a
// developer's startup stays offline. Set Options.AlwaysResolve to resolve the
// release anyway — an interactive caller needs the available version to report
// alongside the reason, or to install past it on request. Only the first
// refusal encountered is reported.
func Check(ctx context.Context, opts Options) (Result, error) {
	res := Result{Current: opts.CurrentVersion, BinPath: opts.BinPath}

	switch {
	case os.Getenv("ANCHORED_NO_AUTOUPDATE") == "1":
		res.Blocked = BlockEnvDisabled
	case opts.CurrentVersion == "":
		res.Blocked = BlockNoVersion
	case IsDevBuild(opts.CurrentVersion):
		res.Blocked = BlockDevBuild
	}

	if res.Blocked == BlockNone || opts.AlwaysResolve {
		if res.BinPath == "" {
			p, err := resolveBinPath()
			if err != nil {
				return res, err
			}
			res.BinPath = p
		}
		if res.Blocked == BlockNone && !underCanonicalBinDir(res.BinPath) {
			res.Blocked = BlockOutsideCanonical
		}
	}

	if res.Blocked != BlockNone && !opts.AlwaysResolve {
		return res, nil
	}
	repo := opts.Repo
	if repo == "" {
		repo = defaultRepo
	}

	tag, err := releaseTag(opts.TargetVersion)
	if err != nil {
		return res, err
	}

	latest, assetURL, assetName, checksumsURL, err := fetchRelease(ctx, repo, tag)
	if err != nil {
		return res, err
	}
	res.Latest = latest
	res.AssetURL = assetURL
	res.AssetName = assetName
	res.ChecksumsURL = checksumsURL
	res.Newer = isNewer(latest, res.Current)

	if res.Blocked == BlockNone && !res.Newer {
		res.Blocked = BlockNotNewer
	}
	return res, nil
}

// Apply installs the release described by res: it looks up the expected
// digest, then downloads, verifies and atomically swaps the binary, keeping
// the previous one at BinPath+".prev".
//
// Apply does not consult res.Blocked. Enforcing policy is the caller's job,
// which is what lets an explicit user request install past a refusal.
func Apply(ctx context.Context, res Result) error {
	if res.AssetURL == "" || res.ChecksumsURL == "" || res.AssetName == "" {
		return errors.New("updater: result carries no resolved release assets")
	}
	if res.BinPath == "" {
		return errors.New("updater: result carries no target binary path")
	}

	sum, err := resolveChecksum(ctx, res.ChecksumsURL, res.AssetURL, res.AssetName)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrChecksumLookup, err)
	}
	return downloadAndReplace(ctx, res.AssetURL, res.BinPath, sum)
}

// osExecutable is a seam for tests: it lets a test drive the
// ErrResolveExecutable path, which os.Executable never reaches under
// `go test`.
var osExecutable = os.Executable

// semverTag constrains what may become a URL path segment.
var semverTag = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// ValidVersionTag reports whether s is shaped like a release version. It is
// exported so a caller can reject a bad value where it is accepted, instead
// of depending on this package's control flow to reach releaseTag.
func ValidVersionTag(s string) bool {
	return semverTag.MatchString(s)
}

// releaseTag normalizes a user-supplied version into the tag GitHub
// publishes, so "0.17.0" and "v0.17.0" both resolve. An empty target means
// the latest release.
//
// SECURITY INVARIANT: the result is interpolated into the release API path,
// and Go transmits dot-segments verbatim rather than normalizing them. A
// value like "../../owner/repo/releases/latest" therefore reaches the server
// intact, and whether it resolves depends entirely on how that one server
// normalizes paths — a property this code neither controls nor should rely
// on. Since the asset and checksums.txt both come from whatever release is
// resolved, they would agree with each other and the install would succeed.
// Validating the shape closes it independently of server behaviour: a
// version that is not a version is a typo or an attack, and neither should
// reach the network.
func releaseTag(target string) (string, error) {
	if target == "" {
		return "", nil
	}
	if !semverTag.MatchString(target) {
		return "", fmt.Errorf("not a version: %q", target)
	}
	return "v" + strings.TrimPrefix(target, "v"), nil
}

// resolveBinPath returns the running binary, symlink-resolved so a wrapper
// path doesn't get swapped in place of the real file.
func resolveBinPath() (string, error) {
	exe, err := osExecutable()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrResolveExecutable, err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// underCanonicalBinDir reports whether binPath lives in ~/.anchored/bin. A
// binary anywhere else is presumed to be a checkout build the owner manages
// by hand, so the background path leaves it alone.
func underCanonicalBinDir(binPath string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	canonical := filepath.Join(home, ".anchored", "bin")
	return strings.HasPrefix(binPath, canonical+string(filepath.Separator))
}
