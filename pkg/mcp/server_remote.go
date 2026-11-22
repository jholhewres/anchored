package mcp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/project"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// resolveRemoteProjectID resolves the remote project a push should target on
// a SPECIFIC remote (explicit `remote: <name>` flows), with the same
// precedence as `remote sync`: the cwd repo's git-origin remote_key first,
// then the entry's linked projects — but the link is only a fallback for
// non-repo contexts, so one global link can't funnel every repo's pushes
// into a single remote project. Returns "" when nothing resolves. The second
// return reports whether the cwd had a matchable git origin.
func (s *Server) resolveRemoteProjectID(ctx context.Context, client *remotesync.Client, entry config.RemoteEntry, cwd string) (string, bool) {
	if s.mem != nil {
		if proj, err := s.mem.ResolveProjectInfo(cwd); err == nil && proj != nil && proj.RemoteKey != "" {
			canonicalKey, legacyKey := project.RemoteKeysFromDir(proj.Path)
			pid, _ := client.ResolveProjectIDByRemoteKeys(ctx, canonicalKey, legacyKey)
			return pid, true
		}
	}
	if len(entry.Projects) > 0 {
		return entry.Projects[0], false
	}
	return "", false
}

// resolveAutoRemoteTarget picks the remote AND the server-side project for
// the automatic read flows (search merge, default-remote search): probe every
// configured remote by the repo's git-origin remote_key — so a
// freshly-configured server works with zero routing setup — falling back to
// the path/default remote's linked project for non-repo contexts. Flows that
// send data use resolveAutoRemoteWriteTarget.
func (s *Server) resolveAutoRemoteTarget(ctx context.Context, cwd string) (*config.RemoteEntry, string) {
	r := s.autoRemoteTarget(ctx, cwd, false, 0)
	return r.Entry, r.ProjectID
}

// resolveAutoRemoteWriteTarget is resolveAutoRemoteTarget for flows that send
// data to the remote (outbox delivery, kg write-through), under strict
// precedence: a held result is a write waiting on a higher-precedence remote
// that is not answering, which it never passes over. budget bounds the lookup
// for a write a person waits on (kg_add); zero for none.
func (s *Server) resolveAutoRemoteWriteTarget(ctx context.Context, cwd string, budget time.Duration) remotesync.ProjectResolution {
	return s.autoRemoteTarget(ctx, cwd, true, budget)
}

// routingMayBeStale reports whether a failed remote call suggests the remote
// is down, so a cached routing answer should be resolved again: a transport
// failure or a 5xx. A 4xx, rate limits included, comes from a server that is
// up.
func routingMayBeStale(err error) bool {
	var remoteErr *remotesync.RemoteError
	if !errors.As(err, &remoteErr) {
		return true
	}
	return remoteErr.StatusCode >= http.StatusInternalServerError
}

// repositoryRemoteKeys returns the canonical and legacy remote keys of the
// git repository at cwd, or ok=false outside one.
func (s *Server) repositoryRemoteKeys(cwd string) (canonicalKey, legacyKey string, ok bool) {
	if s.mem == nil {
		return "", "", false
	}
	proj, err := s.mem.ResolveProjectInfo(cwd)
	if err != nil || proj == nil || proj.RemoteKey == "" {
		return "", "", false
	}
	canonicalKey, legacyKey = project.RemoteKeysFromDir(proj.Path)
	return canonicalKey, legacyKey, true
}

// projectFilterRoot is the project root the remote safety filter lets local
// paths through under, for what a tool sends: the root of the repository the
// write was resolved to, or none outside a repository. A cwd such as the home
// directory must never make every home path look like a project path. It
// reads the project the save already detected, so it costs one query and no
// git call on the save path.
func (s *Server) projectFilterRoot(ctx context.Context, projectID *string) string {
	if s.mem == nil {
		return ""
	}
	return s.mem.ProjectPath(ctx, projectID)
}

// forgetAutoRemoteTarget drops what the cache holds for cwd's repository.
func (s *Server) forgetAutoRemoteTarget(cwd string) {
	if s.cfg == nil {
		return
	}
	if canonicalKey, legacyKey, ok := s.repositoryRemoteKeys(cwd); ok {
		s.remoteResolutions.forget(remoteResolutionKey(s.cfg, cwd, canonicalKey, legacyKey))
	}
}

// forgetAutoRemoteRead drops the read answer cached for cwd's repository and
// leaves any write hold in place.
func (s *Server) forgetAutoRemoteRead(cwd string) {
	if s.cfg == nil {
		return
	}
	if canonicalKey, legacyKey, ok := s.repositoryRemoteKeys(cwd); ok {
		s.remoteResolutions.forgetRead(remoteResolutionKey(s.cfg, cwd, canonicalKey, legacyKey))
	}
}

// holdAutoRemoteTarget replaces what the cache holds for cwd's repository with
// a write hold, so the writes queued behind a refusal are held without asking
// the remote again until the hold expires.
func (s *Server) holdAutoRemoteTarget(cwd string, held remotesync.ProjectResolution) {
	if s.cfg == nil {
		return
	}
	if canonicalKey, legacyKey, ok := s.repositoryRemoteKeys(cwd); ok {
		key := remoteResolutionKey(s.cfg, cwd, canonicalKey, legacyKey)
		s.remoteResolutions.forget(key)
		s.remoteResolutions.put(key+heldKeySuffix, held, remoteMissTTL)
	}
}

func (s *Server) autoRemoteTarget(ctx context.Context, cwd string, write bool, writeBudget time.Duration) remotesync.ProjectResolution {
	if s.cfg == nil {
		return remotesync.ProjectResolution{}
	}
	if canonicalKey, legacyKey, ok := s.repositoryRemoteKeys(cwd); ok {
		return s.resolveProjectAcrossRemotes(ctx, cwd, canonicalKey, legacyKey, write, writeBudget)
	}
	if entry := s.cfg.ResolveRemote(cwd); entry != nil && len(entry.Projects) > 0 {
		return remotesync.ProjectResolution{Entry: entry, ProjectID: entry.Projects[0]}
	}
	return remotesync.ProjectResolution{}
}

// hasAnyRemote reports whether at least one remote server is configured.
// Auto-sync gates use it instead of ResolveRemote: a named-only setup with no
// default flag and no routing paths resolves to nothing, but the origin probe
// inside resolveAutoRemoteTarget still searches every configured remote.
func (s *Server) hasAnyRemote() bool {
	return s.cfg != nil && len(s.cfg.Remotes) > 0
}

// hasReachableRemoteRepositoryProject is stricter than automatic remote
// routing: it intentionally refuses the non-repository linked-project
// fallback. The skill-priority banner is a promise that this repository has a
// live server project, so a stale configured link must not activate it.
func (s *Server) hasReachableRemoteRepositoryProject(ctx context.Context, cwd string) bool {
	target, remoteProjectID := s.resolveReachableRemoteRepositoryProject(ctx, cwd)
	return target != nil && remoteProjectID != ""
}

// resolveReachableRemoteRepositoryProject resolves only through the current
// repository's canonical/legacy remote keys. Unlike resolveAutoRemoteTarget,
// it never falls back to a configured linked project, which could belong to a
// different repository after configuration drift.
func (s *Server) resolveReachableRemoteRepositoryProject(ctx context.Context, cwd string) (*config.RemoteEntry, string) {
	if s.cfg == nil {
		return nil, ""
	}
	canonicalKey, legacyKey, ok := s.repositoryRemoteKeys(cwd)
	if !ok {
		return nil, ""
	}
	r := s.resolveProjectAcrossRemotes(ctx, cwd, canonicalKey, legacyKey, false, 0)
	return r.Entry, r.ProjectID
}

func (s *Server) hasAutoSyncRemote() bool {
	if s.cfg == nil {
		return false
	}
	for _, entry := range s.cfg.Remotes {
		if entry.AutoSyncEnabled() {
			return true
		}
	}
	return false
}

// pushTripleTo pushes a single triple to an already-resolved remote project.
// The caller resolves the target (so the user-facing response can name it);
// the push stays best-effort: failures are logged, never surfaced.
func (s *Server) pushTripleTo(entry config.RemoteEntry, remoteProjID, projectRoot string, t kg.Triple) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := remotesync.NewClientFromEntry(entry, "mcp")
	if _, err := client.PushTriples(ctx, remoteProjID, projectRoot, []remotesync.SyncTriple{remotesync.TripleFromKG(t)}); err != nil {
		s.logger.Warn("kg auto-sync push failed", "remote", entry.Name, "error", err)
	}
}
