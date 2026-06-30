package project

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type Project struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	RemoteKey  string  `json:"remote_key,omitempty"`
	SourceTool *string `json:"source_tool,omitempty"`
}

type Detector struct {
	db *sql.DB
}

func NewDetector(db *sql.DB) *Detector {
	return &Detector{db: db}
}

func (d *Detector) Detect(cwd string) (*Project, error) {
	gitRoot, err := gitRoot(cwd)
	if err != nil || gitRoot == "" {
		return nil, nil
	}

	gitRoot, err = filepath.Abs(gitRoot)
	if err != nil {
		return nil, err
	}

	var existing Project
	err = d.db.QueryRow(
		"SELECT id, name, path, source_tool, COALESCE(remote_key, '') FROM projects WHERE path = ?",
		gitRoot,
	).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.SourceTool, &existing.RemoteKey)

	if err == nil {
		// Backfill or re-key: when the stored key is missing OR no longer
		// matches the canonical (v2) key the origin now derives — e.g. a
		// record keyed with the legacy normalization — update it to the
		// canonical key so local and server keys converge.
		rk := deriveRemoteKey(gitRoot)
		if rk != "" && rk != existing.RemoteKey {
			if _, uerr := d.db.Exec("UPDATE projects SET remote_key = ? WHERE id = ?", rk, existing.ID); uerr != nil {
				slog.Debug("remote_key re-key failed; will retry on next detect", "project_id", existing.ID, "error", uerr)
			}
			existing.RemoteKey = rk
		}
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	name := filepath.Base(gitRoot)
	id := newID()
	rk := deriveRemoteKey(gitRoot)

	_, err = d.db.Exec(
		"INSERT INTO projects (id, name, path, remote_key) VALUES (?, ?, ?, ?)",
		id, name, gitRoot, rk,
	)
	if err != nil {
		return nil, err
	}

	return &Project{ID: id, Name: name, Path: gitRoot, RemoteKey: rk}, nil
}

func (d *Detector) Resolve(id string) (*Project, error) {
	var p Project
	err := d.db.QueryRow(
		"SELECT id, name, path, source_tool, COALESCE(remote_key, '') FROM projects WHERE id = ?",
		id,
	).Scan(&p.ID, &p.Name, &p.Path, &p.SourceTool, &p.RemoteKey)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func gitRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail on a healthy system, but ignoring the
		// error would silently return an all-zero (non-unique) id.
		panic(err)
	}
	return hex.EncodeToString(b)
}

func getGitRemoteURL(cwd string) string {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var (
	hostPortRe = regexp.MustCompile(`^([^/:]+):\d+(/.*)?$`)
	leadingWWW = regexp.MustCompile(`^www\.`)
	leadingGit = regexp.MustCompile(`^git@`)
)

// normalizeRemoteURL reduces various git remote URL formats to a canonical
// form (normalization v2). On top of the legacy pipeline it strips a numeric
// host port and a leading "scm/" path segment, so the same repository reached
// over different protocols/ports collapses to one key:
//
//	https://github.com/user/repo.git                   → github.com/user/repo
//	git@github.com:user/repo.git                       → github.com/user/repo
//	ssh://git@github.com/user/repo                      → github.com/user/repo
//	ssh://git@bitbucket.example.com:7999/proj/repo.git → bitbucket.example.com/proj/repo
//	https://bitbucket.example.com/scm/proj/repo.git    → bitbucket.example.com/proj/repo
func normalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// ssh://git@host/path → host/path
	if strings.HasPrefix(s, "ssh://") {
		s = strings.TrimPrefix(s, "ssh://")
		s = leadingGit.ReplaceAllString(s, "")
	}

	// git@host:path → host/path
	if strings.Contains(s, "@") && strings.Contains(s, ":") {
		parts := strings.SplitN(s, "@", 2)
		if len(parts) == 2 {
			rest := parts[1]
			idx := strings.Index(rest, ":")
			if idx >= 0 {
				s = rest[:idx] + "/" + rest[idx+1:]
			} else {
				s = rest
			}
		}
	}

	// https:// or http:// host/path → host/path
	if strings.HasPrefix(s, "https://") {
		s = strings.TrimPrefix(s, "https://")
	} else if strings.HasPrefix(s, "http://") {
		s = strings.TrimPrefix(s, "http://")
	}

	s = leadingWWW.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "/")
	s = strings.ToLower(s)

	// v2: strip a numeric port from the host (host:7999/path → host/path).
	if m := hostPortRe.FindStringSubmatch(s); m != nil {
		s = m[1] + m[2]
	}

	// v2: strip a leading "scm/" path segment (host/scm/rest → host/rest),
	// only when scm is the FIRST path segment.
	if idx := strings.Index(s, "/"); idx >= 0 {
		host, rest := s[:idx], s[idx+1:]
		if strings.HasPrefix(rest, "scm/") {
			s = host + "/" + strings.TrimPrefix(rest, "scm/")
		}
	}

	return s
}

// normalizeRemoteURLLegacy is the frozen v1 normalization pipeline, kept verbatim
// so legacy remote_keys (computed before v2) can still be derived for fallback
// resolution against servers that haven't re-keyed yet.
func normalizeRemoteURLLegacy(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, ".git")

	// ssh://git@host/path → host/path
	if strings.HasPrefix(s, "ssh://") {
		s = strings.TrimPrefix(s, "ssh://")
		s = regexp.MustCompile(`^git@`).ReplaceAllString(s, "")
	}

	// git@host:path → host/path
	if strings.Contains(s, "@") && strings.Contains(s, ":") {
		parts := strings.SplitN(s, "@", 2)
		if len(parts) == 2 {
			rest := parts[1]
			idx := strings.Index(rest, ":")
			if idx >= 0 {
				s = rest[:idx] + "/" + rest[idx+1:]
			} else {
				s = rest
			}
		}
	}

	// https:// or http:// host/path → host/path
	if strings.HasPrefix(s, "https://") {
		s = strings.TrimPrefix(s, "https://")
	} else if strings.HasPrefix(s, "http://") {
		s = strings.TrimPrefix(s, "http://")
	}

	s = regexp.MustCompile(`^www\.`).ReplaceAllString(s, "")
	s = strings.TrimRight(s, "/")
	s = strings.ToLower(s)

	return s
}

// hashNormalized returns the 16-hex-char SHA-256 prefix of a normalized URL,
// or "" when the input is empty.
func hashNormalized(normalized string) string {
	if normalized == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:8])
}

// DeriveRemoteKeyFromURL returns the canonical (v2) remote_key for a raw git
// remote URL, or "" when empty/unnormalizable. This is the authoritative key
// derivation shared with the sync server mirror.
func DeriveRemoteKeyFromURL(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	return hashNormalized(normalizeRemoteURL(rawURL))
}

// DeriveLegacyRemoteKeyFromURL returns the legacy (v1) remote_key for a raw git
// remote URL, or "" when empty/unnormalizable. Used as a fallback so a project
// registered under the old key on a not-yet-rekeyed server is still found.
func DeriveLegacyRemoteKeyFromURL(rawURL string) string {
	if strings.TrimSpace(rawURL) == "" {
		return ""
	}
	return hashNormalized(normalizeRemoteURLLegacy(rawURL))
}

// deriveRemoteKey returns the canonical 16-hex-char remote_key from the git
// remote URL of cwd, or "" if no remote.
func deriveRemoteKey(cwd string) string {
	return DeriveRemoteKeyFromURL(getGitRemoteURL(cwd))
}

// RemoteKeysFromDir returns the (canonical, legacy) remote_keys for the git
// origin of dir. Either may be "" when there is no origin. Callers probe the
// canonical key first, then the legacy one, so a project still registered
// under the old key on a not-yet-rekeyed server is found.
func RemoteKeysFromDir(dir string) (canonical, legacy string) {
	rawURL := getGitRemoteURL(dir)
	if rawURL == "" {
		return "", ""
	}
	return DeriveRemoteKeyFromURL(rawURL), DeriveLegacyRemoteKeyFromURL(rawURL)
}
