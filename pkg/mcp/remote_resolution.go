package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	remotesync "github.com/jholhewres/anchored/pkg/sync"
)

// The automatic remote flows (search merge, save and kg write-through, the
// skill-priority probe) all ask which remote knows the current repository.
// Answering lists a remote's projects, one round trip per remote asked (just
// the routed one when it knows the repository), so the server keeps the
// answer instead of paying it on every call.
const (
	remoteResolutionTTL = 5 * time.Minute
	// Expired entries are swept once the cache holds this many.
	remoteResolutionSweepAt = 256
)

// remoteReadProbeTimeout bounds each remote's answer on the interactive read
// flows; writes resolve under strict precedence instead.
var remoteReadProbeTimeout = remotesync.InteractiveProbeTimeout

// A miss also stands for a remote that did not answer, and a held write for
// one that is not answering yet. Both are kept long enough that a dead remote
// does not stall every call and short enough that a recovered remote, or a
// project created on a server meanwhile, is found soon.
var remoteMissTTL = time.Minute

// heldKeySuffix keeps held writes apart from read answers in the cache.
const heldKeySuffix = "\x00held"

type remoteResolution struct {
	r       remotesync.ProjectResolution
	expires time.Time
}

type remoteResolutionCache struct {
	mu      sync.Mutex
	entries map[string]remoteResolution
}

// get returns an unexpired resolution, with its own copy of the entry.
func (c *remoteResolutionCache) get(key string) (remotesync.ProjectResolution, bool) {
	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()
	if !ok || !time.Now().Before(cached.expires) {
		return remotesync.ProjectResolution{}, false
	}
	r := cached.r
	if r.Entry != nil {
		entry := *r.Entry
		r.Entry = &entry
	}
	return r, true
}

func (c *remoteResolutionCache) put(key string, r remotesync.ProjectResolution, ttl time.Duration) {
	if r.Entry != nil {
		entry := *r.Entry
		r.Entry = &entry
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]remoteResolution)
	}
	if len(c.entries) >= remoteResolutionSweepAt {
		now := time.Now()
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = remoteResolution{r: r, expires: time.Now().Add(ttl)}
}

// forgetRead drops the read answer cached under key and keeps any hold.
func (c *remoteResolutionCache) forgetRead(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// forget drops the read answer and any held write cached under key.
func (c *remoteResolutionCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	delete(c.entries, key+heldKeySuffix)
}

// resolveProjectAcrossRemotes is remotesync's read resolver
// (ResolveProjectAcrossRemotesDetailed) or write resolver
// (ResolveProjectAcrossRemotesForWrite) behind the server's cache. The key covers the repository's keys, the remote
// the cwd routes to and every configured remote, so a configuration change
// never answers from an entry made for another one. Reads probe under the
// interactive deadline and may fall back past a remote that did not answer.
// Writes never reuse such a degraded answer: they resolve under strict
// precedence, and a held write is kept for remoteMissTTL so the writes queued
// behind it do not probe the same remote again.
func (s *Server) resolveProjectAcrossRemotes(ctx context.Context, cwd, canonicalKey, legacyKey string, write bool) remotesync.ProjectResolution {
	key := remoteResolutionKey(s.cfg, cwd, canonicalKey, legacyKey)
	c := &s.remoteResolutions
	if write {
		// A hold outranks a read answer cached after it: a search that
		// lists the projects again does not lift a refusal.
		if r, ok := c.get(key + heldKeySuffix); ok {
			return r
		}
	}
	if r, ok := c.get(key); ok && (!write || !r.Degraded) {
		return r
	}

	var r remotesync.ProjectResolution
	if write {
		r = remotesync.ResolveProjectAcrossRemotesForWrite(ctx, s.cfg, cwd, "mcp", canonicalKey, legacyKey)
	} else {
		r = remotesync.ResolveProjectAcrossRemotesDetailed(ctx, s.cfg, cwd, "mcp", remoteReadProbeTimeout, canonicalKey, legacyKey)
	}
	switch {
	case ctx.Err() != nil:
		// Cut short by the caller, not by the remotes: nothing to keep.
	case r.Held():
		c.put(key+heldKeySuffix, r, remoteMissTTL)
	case r.Entry != nil && r.ProjectID != "" && !r.Degraded:
		c.put(key, r, remoteResolutionTTL)
	default:
		// A miss, or an answer given while a remote ranked above it did not
		// answer: ask again soon so precedence comes back once it does.
		c.put(key, r, remoteMissTTL)
	}
	return r
}

func remoteResolutionKey(cfg *config.Config, cwd, canonicalKey, legacyKey string) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, part := range parts {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	write(canonicalKey, legacyKey)
	if routed := cfg.ResolveRemote(cwd); routed != nil {
		write("routed", routed.Name)
	}
	for _, name := range cfg.RemoteNames() {
		entry := cfg.Remotes[name]
		write("remote", name, entry.ServerURL, entry.APIKey)
	}
	return hex.EncodeToString(h.Sum(nil))
}
