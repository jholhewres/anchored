package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// InteractiveProbeTimeout bounds each remote's answer when a person is
// waiting on the result, so a remote that hangs is skipped instead of holding
// it until the HTTP client gives up.
const InteractiveProbeTimeout = 2500 * time.Millisecond

var remoteProbeTimeout = InteractiveProbeTimeout

// remoteWriteProbeTimeout bounds each remote's answer when resolving a write.
// It is generous, so a slow remote keeps its precedence, yet well below the
// outbox worker's 30 s lease, so a remote that hangs cannot stall the queue.
// Foreground writers (CLI save and sync) wait up to this per remote ranked
// above the match; kg_add bounds the whole resolution with its own deadline.
var remoteWriteProbeTimeout = 10 * time.Second

// remoteHedgeDelay is how long the other remotes wait for the routed one
// before they are asked too. A routed remote that knows the repository and
// answers within it is the only server contacted.
var remoteHedgeDelay = time.Second

// ProjectResolution is the remote and project a repository resolved to.
type ProjectResolution struct {
	Entry      *config.RemoteEntry
	ProjectID  string
	MatchedKey string
	// Degraded reports that a remote ranked above the answer failed to
	// answer, so the answer may change once that remote is back.
	Degraded bool
	// HeldBy names the remote a write is held behind, and HeldStatus is its
	// HTTP status, or 0 when it did not answer at all.
	HeldBy     string
	HeldStatus int
}

// Held reports that a write resolved to nothing because a higher-precedence
// remote did not answer: it waits for that remote rather than going elsewhere.
// Only ResolveProjectAcrossRemotesForWrite holds; a read that finds nothing
// while a remote failed is a degraded miss, not a hold.
func (r ProjectResolution) Held() bool {
	return r.HeldBy != ""
}

// HeldReason explains a held write for the person: who held it, and what to
// do when the remote refused the credentials.
func (r ProjectResolution) HeldReason() string {
	switch {
	case !r.Held():
		return ""
	case r.HeldStatus == http.StatusUnauthorized || r.HeldStatus == http.StatusForbidden:
		return fmt.Sprintf("remote %q refused the credentials (HTTP %d); log in again, writes for this repository are held until then", r.HeldBy, r.HeldStatus)
	case r.HeldStatus != 0:
		return fmt.Sprintf("remote %q did not answer (HTTP %d); writes for this repository are held until it does", r.HeldBy, r.HeldStatus)
	default:
		return fmt.Sprintf("remote %q did not answer; writes for this repository are held until it does", r.HeldBy)
	}
}

// ResolveProjectAcrossRemotes finds which configured remote knows the given
// git-origin remote_key(s), returning that remote, the server-side project ID,
// and the key that matched on it.
//
// This is what makes a freshly-configured second server work with zero extra
// setup: `remote configure --name company` adds an entry with no routing
// paths, so path/default resolution alone would keep sending a company repo
// to the personal server (which rejects the unknown key). The probe asks each
// configured remote "do you know this repository?" and routes to the one that
// does.
//
// Each remote is probed with every key in order (canonical first, then the
// legacy key) so a project still registered under the old normalization on a
// not-yet-rekeyed server is found.
//
// Precedence is deterministic: the path/default-resolved remote for cwd
// first, then the remaining remotes by name. A path glob that matches cwd
// limits the probe to its own remote (see remotesToAsk). The first remote is
// asked at once; the others are asked too once it is late (remoteHedgeDelay)
// or answers without the repository. The first remote in precedence order
// that knows a key wins, so a known repository costs one request and an
// unknown one costs the slowest remote instead of the sum of all of them.
// Each answer is bounded by the interactive deadline; remotes that fail to
// answer are skipped — best-effort. Returns (nil, "", "") when no remote
// knows any key.
func ResolveProjectAcrossRemotes(ctx context.Context, cfg *config.Config, cwd, clientID string, keys ...string) (*config.RemoteEntry, string, string) {
	r := ResolveProjectAcrossRemotesDetailed(ctx, cfg, cwd, clientID, remoteProbeTimeout, keys...)
	return r.Entry, r.ProjectID, r.MatchedKey
}

// ResolveProjectAcrossRemotesForWrite is ResolveProjectAcrossRemotes for flows
// that send data, under strict precedence. Remotes are asked one at a time in
// precedence order, each for up to remoteWriteProbeTimeout, so a slow server
// keeps its precedence. A remote that fails to answer holds the
// write (Held) instead of passing it to a remote ranked below, because it may
// own the repository.
func ResolveProjectAcrossRemotesForWrite(ctx context.Context, cfg *config.Config, cwd, clientID string, keys ...string) ProjectResolution {
	if cfg == nil || !anyKey(keys) {
		return ProjectResolution{}
	}
	for _, e := range remotesToAsk(cfg, cwd) {
		pctx, cancel := context.WithTimeout(ctx, remoteWriteProbeTimeout)
		pid, matched, err := NewClientFromEntry(*e, clientID).lookupProjectIDByRemoteKeys(pctx, keys...)
		cancel()
		if err != nil {
			// This remote may own the repository: hold rather than pass
			// over it, and leave the rest unasked.
			held := ProjectResolution{Degraded: true, HeldBy: e.Name}
			var remoteErr *RemoteError
			if errors.As(err, &remoteErr) {
				held.HeldStatus = remoteErr.StatusCode
			}
			return held
		}
		if pid != "" {
			return ProjectResolution{Entry: e, ProjectID: pid, MatchedKey: matched}
		}
	}
	return ProjectResolution{}
}

// ResolveProjectAcrossRemotesDetailed is ResolveProjectAcrossRemotes that also
// reports whether a higher-precedence remote failed to answer, so a caller
// that caches the answer can keep it only briefly. probeTimeout bounds each
// remote's answer; zero leaves only ctx and the HTTP client's timeout.
func ResolveProjectAcrossRemotesDetailed(ctx context.Context, cfg *config.Config, cwd, clientID string, probeTimeout time.Duration, keys ...string) ProjectResolution {
	if cfg == nil || !anyKey(keys) {
		return ProjectResolution{}
	}
	ordered := remotesToAsk(cfg, cwd)

	type probe struct {
		pid, matched string
		err          error
	}
	// hedge ends when the others may be asked: after remoteHedgeDelay, or as
	// soon as the first remote answers without the repository.
	hedge, openHedge := context.WithCancel(context.Background())
	defer openHedge()
	timer := time.AfterFunc(remoteHedgeDelay, openHedge)
	defer timer.Stop()

	// Answering returns before every probe is done; the rest stop then.
	// Deferred after the hedge, so it runs first: a probe released by the
	// hedge on the way out sees its context already cancelled.
	ctx, stopProbes := context.WithCancel(ctx)
	defer stopProbes()

	answers := make([]chan probe, len(ordered))
	for i, e := range ordered {
		answers[i] = make(chan probe, 1)
		go func(first bool, e config.RemoteEntry, answer chan<- probe) {
			if !first {
				select {
				case <-hedge.Done():
				case <-ctx.Done():
				}
				// An answer may have opened the hedge on its way out.
				if err := ctx.Err(); err != nil {
					answer <- probe{err: err}
					return
				}
			}
			pctx := ctx
			if probeTimeout > 0 {
				var probeCancel context.CancelFunc
				pctx, probeCancel = context.WithTimeout(ctx, probeTimeout)
				defer probeCancel()
			}
			pid, matched, err := NewClientFromEntry(e, clientID).lookupProjectIDByRemoteKeys(pctx, keys...)
			if first && pid == "" {
				openHedge()
			}
			answer <- probe{pid, matched, err}
		}(i == 0, *e, answers[i])
	}
	degraded := false
	for i, answer := range answers {
		p := <-answer
		if p.pid != "" {
			return ProjectResolution{Entry: ordered[i], ProjectID: p.pid, MatchedKey: p.matched, Degraded: degraded}
		}
		if p.err != nil {
			degraded = true
		}
	}
	return ProjectResolution{Degraded: degraded}
}

// remotesToAsk is remotesInPrecedence cut to the routed remote when a path
// glob matches cwd. An explicit glob names the repository's remote outright,
// for reads and writes. Keys derive from the public origin URL, so another
// server that lists a project under the same key must neither take the
// writes nor answer for the repository.
func remotesToAsk(cfg *config.Config, cwd string) []*config.RemoteEntry {
	order := remotesInPrecedence(cfg, cwd)
	if _, pattern := cfg.ResolveRemoteWithPattern(cwd); pattern != "" && len(order) > 0 {
		return order[:1]
	}
	return order
}

// remotesInPrecedence lists the configured remotes in the order a repository
// resolves to them: the path/default-resolved remote for cwd, then the rest by
// name.
func remotesInPrecedence(cfg *config.Config, cwd string) []*config.RemoteEntry {
	ordered := make([]*config.RemoteEntry, 0, len(cfg.Remotes)+1)
	seen := map[string]bool{}
	if e := cfg.ResolveRemote(cwd); e != nil {
		ordered = append(ordered, e)
		seen[e.Name] = true
	}
	for _, name := range cfg.RemoteNames() {
		if seen[name] {
			continue
		}
		e := cfg.Remotes[name]
		e.Name = name
		ordered = append(ordered, &e)
	}
	return ordered
}

func anyKey(keys []string) bool {
	for _, k := range keys {
		if k != "" {
			return true
		}
	}
	return false
}
