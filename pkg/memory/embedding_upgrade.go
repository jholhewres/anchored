package memory

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Upgrading the embedding pipeline without switching semantic search off.
//
// Vectors up to v0.19 were built by the legacy pipeline (a tokenizer that
// misread the model's tokenizer.json). The v2 pipeline embeds the same model
// correctly, so its vectors live in a different semantic space: a query
// embedded one way cannot be scored against vectors embedded the other. While
// the v2 generation is built, the active legacy generation keeps answering
// queries through the legacy pipeline, and every save is embedded into both.
// The v2 generation replaces it only when it covers every live memory, its
// space is healthy, and no binary older than v0.20 holds the database: such a
// binary could only query the legacy space.

// LegacyPipelineProvider is a provider that can hand out the legacy pipeline
// of its model, sharing the loaded model.
type LegacyPipelineProvider interface {
	EmbeddingProvider
	LegacyPipeline() (EmbeddingProvider, error)
}

// LegacyPipeline implements LegacyPipelineProvider.
func (e *ONNXEmbedder) LegacyPipeline() (EmbeddingProvider, error) {
	if e.legacy {
		return nil, fmt.Errorf("onnx: embedder already runs the legacy pipeline")
	}
	return e.Pipeline(true)
}

// embeddingJobKindV2 names the jobs that build a generation for the v2
// pipeline. Binaries before v0.20 claim only embeddingJobKind, so they never
// take one of these and mark it done without a vector.
const embeddingJobKindV2 = "embedding_v2"

// EmbeddingHealthMaxMean is the highest mean cosine between random pairs of
// document vectors a generation may show and still replace the active one. A
// collapsed space (the legacy pipeline measured 0.73 on a real corpus) ranks
// everything alike; a healthy one measured 0.23 over that whole corpus and
// 0.35 over its curated memories alone, which are all one person's notes on
// a few subjects, so the limit leaves room for a homogeneous corpus.
const EmbeddingHealthMaxMean = 0.45

// embeddingHealthSample is how many vectors the health check draws.
const embeddingHealthSample = 300

// EmbeddingSampleStore samples a generation's document vectors.
type EmbeddingSampleStore interface {
	SampleEmbeddingVectors(ctx context.Context, generationID string, n int) ([][]float32, error)
}

// MeanPairwiseCosine is the mean cosine over every pair of vecs (0 for fewer
// than two).
func MeanPairwiseCosine(vecs [][]float32) float64 {
	var sum float64
	pairs := 0
	for i := 0; i < len(vecs); i++ {
		for j := i + 1; j < len(vecs); j++ {
			sum += cosineSimilarityFloat32(vecs[i], vecs[j])
			pairs++
		}
	}
	if pairs == 0 {
		return 0
	}
	return sum / float64(pairs)
}

// upgradeGate decides whether the building generation may replace the active
// one. It runs after every embedding job while the generation builds, so the
// checks go cheapest first: jobs still pending, a memory still without a
// vector, the hold switch, the processes holding the database, and last the
// health of the space (cached per generation for healthRecheckEvery). It
// returns "" when every gate passes, or why it holds; health is the measured
// mean cosine (NaN when not measured).
func (s *Service) upgradeGate(ctx context.Context, generations EmbeddingGenerationStore, generation *EmbeddingGeneration) (reason string, health float64, err error) {
	health = math.NaN()
	id := generation.ID
	if probe, ok := s.store.(embeddingBuildProbe); ok {
		pending, err := probe.HasPendingEmbeddingJobs(ctx, id)
		if err != nil {
			return "", health, err
		}
		if pending {
			return "embedding jobs still pending", health, nil
		}
		missing, err := probe.HasMissingEmbeddingRevisions(ctx, id)
		if err != nil {
			return "", health, err
		}
		if missing {
			return "live memories still missing a vector", health, nil
		}
	} else if pending, err := generations.ListMissingEmbeddingRevisions(ctx, id, 1); err != nil {
		return "", health, err
	} else if len(pending) > 0 {
		return "live memories still missing a vector", health, nil
	}
	if s.holdEmbeddingUpgrade {
		return "held by embedding.hold_upgrade", health, nil
	}
	if gate, ok := s.store.(*SQLiteStore); ok && gate.Path() != "" {
		holders, verified, err := gate.LegacyHoldersVerified(ctx, gate.Path(), IrreversibleStepsMinVersion)
		if err != nil {
			return "", health, err
		}
		if len(holders) > 0 {
			return fmt.Sprintf("%d process(es) older than %s hold the database: %v",
				len(holders), IrreversibleStepsMinVersion, holders), health, nil
		}
		if !verified && !s.confirmEmbeddingUpgrade {
			// Binaries before v0.20 never register, and without /proc they
			// cannot be found by the files they hold open.
			return "cannot see which processes hold the database on this platform; once every client runs v0.20, set embedding.confirm_upgrade: true", health, nil
		}
	}
	health, sampled, err := s.generationHealth(ctx, generation)
	if err != nil {
		return "", math.NaN(), err
	}
	if math.IsNaN(health) {
		if sampled >= 2 {
			// Vectors exist but too few are usable (non-finite, wrong size).
			return fmt.Sprintf("vector space health could not be measured: %d sampled vectors, fewer than 2 usable", sampled), health, nil
		}
		return "", health, nil // under two memories: nothing to measure
	}
	if !(health <= EmbeddingHealthMaxMean) {
		return fmt.Sprintf("vector space too collapsed (mean cosine %.3f > %.2f)", health, EmbeddingHealthMaxMean), health, nil
	}
	return "", health, nil
}

// embeddingBuildProbe answers the gate's cheap questions without scanning
// every live memory.
type embeddingBuildProbe interface {
	HasPendingEmbeddingJobs(ctx context.Context, generationID string) (bool, error)
	HasMissingEmbeddingRevisions(ctx context.Context, generationID string) (bool, error)
}

// healthRecheckEvery is how long a measured health is reused for the same
// generation: the gate would otherwise sample after every job while held.
const healthRecheckEvery = 10 * time.Minute

// generationHealth measures (or reuses) the mean pairwise cosine of a sample
// of the generation's vectors, and how many vectors the sample drew. Vectors
// of the wrong dimension or with a non-finite component are dropped: they
// would bend the mean either way. NaN when fewer than two usable vectors
// exist.
func (s *Service) generationHealth(ctx context.Context, generation *EmbeddingGeneration) (float64, int, error) {
	s.healthMu.Lock()
	if s.healthGen == generation.ID && time.Since(s.healthAt) < healthRecheckEvery {
		v, n := s.healthVal, s.healthSampled
		s.healthMu.Unlock()
		return v, n, nil
	}
	s.healthMu.Unlock()
	sampler, ok := s.store.(EmbeddingSampleStore)
	if !ok {
		return math.NaN(), 0, nil
	}
	vecs, err := sampler.SampleEmbeddingVectors(ctx, generation.ID, embeddingHealthSample)
	if err != nil {
		return math.NaN(), 0, err
	}
	sampled := len(vecs)
	usable := vecs[:0]
	for _, v := range vecs {
		if len(v) == generation.Identity.Dimensions && finiteVector(v) {
			usable = append(usable, v)
		}
	}
	health := math.NaN()
	if len(usable) >= 2 {
		health = MeanPairwiseCosine(usable)
	}
	s.healthMu.Lock()
	s.healthGen, s.healthAt, s.healthVal, s.healthSampled = generation.ID, time.Now(), health, sampled
	s.healthMu.Unlock()
	return health, sampled, nil
}

func finiteVector(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

// setServedGeneration records which generation answers this process's
// queries ("" when semantic search is off).
func (s *Service) setServedGeneration(generationID string) {
	s.embeddingMu.Lock()
	s.servedGenID = generationID
	s.embeddingMu.Unlock()
}

func (s *Service) servedGeneration() string {
	s.embeddingMu.RLock()
	defer s.embeddingMu.RUnlock()
	return s.servedGenID
}

// generationCheckEvery is how often a worker compares the generation it
// serves with the one active in the database. Another process activates the
// new generation, or an older binary revives the legacy one; either way this
// process must rebind or it keeps answering from a stale space.
const generationCheckEvery = 30 * time.Second

// activationRetryEvery is how often an idle process retries a held switch.
// The gate's coverage probe scans every live memory once the new generation
// is complete, so it is not repeated at every generation check.
const activationRetryEvery = 5 * time.Minute

// checkActiveGeneration rebinds this process when the database's active
// generation is no longer the one it serves.
func (s *Service) checkActiveGeneration(ctx context.Context) {
	generations, ok := s.store.(EmbeddingGenerationStore)
	if !ok || s.embedder == nil {
		return
	}
	active, err := generations.ActiveEmbeddingGeneration(ctx)
	if err != nil {
		return
	}
	activeID := ""
	if active != nil {
		activeID = active.ID
	}
	if activeID == s.servedGeneration() {
		// Still on the same generation. A held switch is otherwise retried
		// only after an embedding job, so an idle process would never notice
		// that the old binary exited or hold_upgrade was turned off.
		target := s.currentEmbeddingGenerationID()
		if target != "" && target != activeID && time.Since(s.lastActivationTry) >= activationRetryEvery {
			s.lastActivationTry = time.Now()
			if _, err := s.tryActivateEmbeddingGeneration(ctx, generations, target); err != nil {
				s.logger.Warn("retry of the held embedding upgrade failed", "error", err)
			}
		}
		return
	}
	s.logger.Info("active embedding generation changed; rebinding",
		"serving", s.servedGeneration(), "active", activeID)
	if err := s.ensureCurrentEmbeddingGeneration(ctx); err != nil {
		s.logger.Warn("rebind to the active embedding generation failed", "error", err)
	}
}

// providerFor returns the configured provider able to embed for identity: the
// current one, or the legacy pipeline serving the active legacy generation.
func (s *Service) providerFor(identity EmbeddingIdentity) EmbeddingProvider {
	s.embeddingMu.RLock()
	defer s.embeddingMu.RUnlock()
	if s.embeddingGenID != "" && identity.Compatible(s.embeddingID) {
		return s.embedder
	}
	if s.legacyEmbedder != nil && identity.Compatible(s.legacyID) {
		return s.legacyEmbedder
	}
	return nil
}

// legacyProviderFor returns the legacy pipeline when active was built by it,
// creating it on first use. nil when the embedder has no legacy pipeline or
// active belongs to another space.
func (s *Service) legacyProviderFor(active *EmbeddingGeneration) EmbeddingProvider {
	if active == nil {
		return nil
	}
	s.embeddingMu.Lock()
	defer s.embeddingMu.Unlock()
	if s.legacyEmbedder != nil {
		if active.Identity.Compatible(s.legacyID) {
			return s.legacyEmbedder
		}
		return nil
	}
	if !s.runsV2Pipeline() {
		return nil
	}
	legacy, err := s.embedder.(LegacyPipelineProvider).LegacyPipeline()
	if err != nil {
		s.logger.Warn("legacy embedding pipeline unavailable", "error", err)
		return nil
	}
	identity, err := EmbeddingIdentityOf(legacy)
	if err != nil || !active.Identity.Compatible(identity) {
		_ = legacy.Close()
		return nil
	}
	s.legacyEmbedder, s.legacyID, s.legacyGenID = legacy, identity, active.ID
	return legacy
}

// retireLegacyPipeline drops the legacy pipeline once its generation is no
// longer active.
func (s *Service) retireLegacyPipeline() {
	s.embeddingMu.Lock()
	legacy := s.legacyEmbedder
	s.legacyEmbedder, s.legacyID, s.legacyGenID = nil, EmbeddingIdentity{}, ""
	s.embeddingMu.Unlock()
	if legacy != nil {
		_ = legacy.Close()
	}
}

func (s *Service) legacyGenerationID() string {
	s.embeddingMu.RLock()
	defer s.embeddingMu.RUnlock()
	return s.legacyGenID
}

// jobKindFor names the durable job kind that embeds into generationID: the
// generation served by the legacy pipeline keeps the kind older binaries know,
// the one the v2 pipeline builds uses its own.
func (s *Service) jobKindFor(generationID string) string {
	if s.runsV2Pipeline() && generationID != s.legacyGenerationID() {
		return embeddingJobKindV2
	}
	return embeddingJobKind
}

// runsV2Pipeline reports whether the embedder is a current pipeline that has
// a legacy one: a legacy pipeline also offers LegacyPipeline (and fails it).
func (s *Service) runsV2Pipeline() bool {
	lp, ok := s.embedder.(LegacyPipelineProvider)
	if !ok {
		return false
	}
	if l, ok := lp.(interface{ Legacy() bool }); ok && l.Legacy() {
		return false
	}
	return true
}

// EmbeddingUpgradeStatus reports the state of an embedding pipeline upgrade.
type EmbeddingUpgradeStatus struct {
	ActiveGeneration   string
	ServedByLegacy     bool
	BuildingGeneration string
	Missing            int
	Health             float64 // mean pairwise cosine; NaN when not measured
	HeldBecause        string  // why the building generation is not active yet
}

// EmbeddingUpgrade reports whether a generation is being built to replace the
// active one and what holds it.
func (s *Service) EmbeddingUpgrade(ctx context.Context) (EmbeddingUpgradeStatus, error) {
	st := EmbeddingUpgradeStatus{Health: math.NaN()}
	generations, ok := s.store.(EmbeddingGenerationStore)
	if !ok || s.embedder == nil {
		return st, nil
	}
	active, err := generations.ActiveEmbeddingGeneration(ctx)
	if err != nil {
		return st, err
	}
	if active != nil {
		st.ActiveGeneration = active.ID
		st.ServedByLegacy = active.ID == s.legacyGenerationID()
	}
	target := s.currentEmbeddingGenerationID()
	if target == "" || (active != nil && active.ID == target) {
		return st, nil
	}
	st.BuildingGeneration = target
	if st.Missing, err = generations.CountMissingEmbeddingRevisions(ctx, target); err != nil {
		return st, err
	}
	gen, err := generations.EmbeddingGeneration(ctx, target)
	if err != nil || gen == nil {
		return st, err
	}
	st.HeldBecause, st.Health, err = s.upgradeGate(ctx, generations, gen)
	return st, err
}

// upgradeLog rate-limits the "upgrade held" log line to one per reason change.
type upgradeLog struct {
	mu   sync.Mutex
	last string
	at   time.Time
}

func (l *upgradeLog) changed(reason string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason == l.last && time.Since(l.at) < time.Hour {
		return false
	}
	l.last, l.at = reason, time.Now()
	return true
}
