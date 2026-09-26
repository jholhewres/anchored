package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bowEmbedder hashes words into a 64-dimension bag of words: texts sharing
// words are close, unrelated ones nearly orthogonal. The seed picks the space,
// so a legacy and a v2 pipeline of the same model disagree the way the real
// ones do (same model, different ModelRevision).
type bowEmbedder struct {
	seed      uint32
	revision  string
	legacy    *bowEmbedder
	collapsed bool
}

func (e *bowEmbedder) vector(text string) []float32 {
	v := make([]float32, 64)
	if e.collapsed {
		v[0] = 1
		return v
	}
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = fmt.Fprintf(h, "%d:%s", e.seed, w)
		v[h.Sum32()%64]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func (e *bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.vector(t)
	}
	return out, nil
}
func (e *bowEmbedder) Dimensions() int       { return 64 }
func (e *bowEmbedder) Name() string          { return "bow" }
func (e *bowEmbedder) Model() string         { return "bow-multilingual" }
func (e *bowEmbedder) ModelRevision() string { return e.revision }
func (e *bowEmbedder) Close() error          { return nil }
func (e *bowEmbedder) Legacy() bool          { return e.legacy == nil }
func (e *bowEmbedder) LegacyPipeline() (EmbeddingProvider, error) {
	if e.legacy == nil {
		return nil, fmt.Errorf("no legacy pipeline")
	}
	return e.legacy, nil
}

var upgradeCorpus = map[string]string{
	"rate":  "the gateway limits requests with a token bucket in redis",
	"auth":  "jwt tokens are validated locally against the cached jwks",
	"retry": "the mobile app retries once after two seconds on timeout",
	"logs":  "services log json with slog and never log request bodies",
}

// legacyDatabase builds what a v0.19 installation has: every memory embedded
// by the legacy pipeline, in the active generation.
func legacyDatabase(t *testing.T) (*SQLiteStore, *bowEmbedder, string) {
	t.Helper()
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for id, content := range upgradeCorpus {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Source: "test", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &bowEmbedder{seed: 1, revision: "legacy"}
	old := newGenerationTestService(t, store, legacy)
	if _, err := old.backfillEmbeddingGeneration(ctx, store, 10, 0, 0); err != nil {
		t.Fatal(err)
	}
	active, err := store.ActiveEmbeddingGeneration(ctx)
	if err != nil || active == nil {
		t.Fatalf("legacy generation not active: %v %v", active, err)
	}
	return store, legacy, active.ID
}

func drainUpgrade(t *testing.T, svc *Service) {
	t.Helper()
	svc.workerOwner = "upgrade-test-worker" // ensureDurableWorkers sets it in production
	for i := 0; svc.drainDurableWork(); i++ {
		if i == 200 {
			t.Fatal("durable work still busy after 200 drains")
		}
	}
}

func vectorHits(t *testing.T, svc *Service, query string) []string {
	t.Helper()
	res, err := svc.searcher.searchVector(context.Background(), query, 5, nil, SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return resultIDs(res)
}

// R13: from the moment v0.20 starts until the v2 generation replaces the
// legacy one, vector search answers — through the legacy pipeline — and then
// switches to the v2 pipeline with nothing in between.
func TestEmbeddingUpgrade_LegacyServesUntilTheNewGenerationTakesOver(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)

	v2 := &bowEmbedder{seed: 2, revision: "v2", legacy: legacy}
	svc := newGenerationTestService(t, store, v2)

	if hits := vectorHits(t, svc, "token bucket redis gateway"); len(hits) == 0 || hits[0] != "rate" {
		t.Fatalf("vector search must answer from the legacy generation right away, got %v", hits)
	}
	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ServedByLegacy || st.ActiveGeneration != legacyGen || st.BuildingGeneration == "" || st.Missing != len(upgradeCorpus) {
		t.Fatalf("upgrade status before the build: %+v", st)
	}

	// A new save goes into both generations, the v2 one under its own kind.
	specs := svc.durableProcessingSpecs(false)
	kinds := map[string]string{}
	for _, sp := range specs {
		kinds[sp.Generation] = sp.Kind
	}
	if kinds[legacyGen] != embeddingJobKind || kinds[st.BuildingGeneration] != embeddingJobKindV2 {
		t.Fatalf("save job specs %+v", specs)
	}

	// A pre-0.20 worker only claims the old kind: never a v2 job.
	for {
		job, err := store.ClaimProcessingJob(ctx, embeddingJobKind, "v0.19-worker", time.Now().UTC(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			break
		}
		if job.Generation != legacyGen {
			t.Fatalf("an old worker claimed a job of generation %s", job.Generation)
		}
		if err := store.CompleteProcessingJob(ctx, job.ID, "v0.19-worker", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	drainUpgrade(t, svc)

	active, err := store.ActiveEmbeddingGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != st.BuildingGeneration {
		t.Fatalf("the v2 generation should be active once complete and healthy, active=%v", active)
	}
	old, err := store.EmbeddingGeneration(ctx, legacyGen)
	if err != nil || old.State != EmbeddingGenerationRetired {
		t.Fatalf("legacy generation should be retired: %+v %v", old, err)
	}
	if svc.legacyGenerationID() != "" {
		t.Error("the legacy pipeline must be dropped after the switch")
	}
	if hits := vectorHits(t, svc, "token bucket redis gateway"); len(hits) == 0 || hits[0] != "rate" {
		t.Fatalf("vector search after the switch, got %v", hits)
	}
}

// No v0.19 process may be holding the database when the switch happens: it
// could only query the legacy space.
func TestEmbeddingUpgrade_WaitsForOlderBinariesToExit(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	old := ProcessInfo{PID: 4242, Host: "other-host", Version: "0.19.2", Role: "serve",
		StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC()}
	if err := store.RegisterProcess(ctx, old); err != nil {
		t.Fatal(err)
	}

	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	drainUpgrade(t, svc)

	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveGeneration != legacyGen || st.Missing != 0 || !strings.Contains(st.HeldBecause, "older than") {
		t.Fatalf("complete v2 generation must wait for the old binary: %+v", st)
	}
	if hits := vectorHits(t, svc, "jwks jwt"); len(hits) == 0 || hits[0] != "auth" {
		t.Fatalf("search must keep answering while held, got %v", hits)
	}

	if err := store.UnregisterProcess(ctx, old.Host, old.PID); err != nil {
		t.Fatal(err)
	}
	if ok, err := svc.tryActivateEmbeddingGeneration(ctx, store, st.BuildingGeneration); err != nil || !ok {
		t.Fatalf("activation after the old binary exited: ok=%v err=%v", ok, err)
	}
}

// A collapsed space ranks everything alike: it never replaces a working one.
func TestEmbeddingUpgrade_RefusesACollapsedSpace(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy, collapsed: true})
	drainUpgrade(t, svc)

	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveGeneration != legacyGen || !strings.Contains(st.HeldBecause, "collapsed") || st.Health < 0.99 {
		t.Fatalf("collapsed v2 space must be held: %+v", st)
	}
}

func TestEmbeddingUpgrade_HoldUpgradeKeepsTheLegacyGeneration(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	svc.holdEmbeddingUpgrade = true
	drainUpgrade(t, svc)
	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveGeneration != legacyGen || !strings.Contains(st.HeldBecause, "hold_upgrade") {
		t.Fatalf("hold_upgrade must keep the legacy generation active: %+v", st)
	}
}

// A new installation has nothing to keep serving: it builds with the v2
// pipeline only and never loads the legacy one.
func TestEmbeddingUpgrade_FreshDatabaseUsesOnlyTheNewPipeline(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for id, content := range upgradeCorpus {
		if err := store.Save(ctx, Memory{ID: id, Category: "fact", Source: "test", Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &bowEmbedder{seed: 1, revision: "legacy"}
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	if svc.legacyGenerationID() != "" {
		t.Fatal("a fresh database must not load the legacy pipeline")
	}
	drainUpgrade(t, svc)
	active, err := store.ActiveEmbeddingGeneration(ctx)
	if err != nil || active == nil || active.Identity.ModelRevision != "v2" {
		t.Fatalf("fresh database active generation: %+v %v", active, err)
	}
}

// doctor and stats read the upgrade from the database alone.
func TestReportEmbeddingGenerations_ShowsTheUpgradeProgress(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})

	reports, err := ReportEmbeddingGenerations(ctx, store.DB(), 100)
	if err != nil {
		t.Fatal(err)
	}
	n := len(upgradeCorpus)
	if len(reports) != 2 || reports[0].ID != legacyGen || reports[0].State != EmbeddingGenerationActive ||
		reports[0].Vectors != n || math.IsNaN(reports[0].Health) ||
		reports[1].State != EmbeddingGenerationBuilding || reports[1].Vectors != 0 || reports[1].Live != n {
		t.Fatalf("before the build: %+v", reports)
	}

	drainUpgrade(t, svc)
	reports, err = ReportEmbeddingGenerations(ctx, store.DB(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].State != EmbeddingGenerationActive || reports[0].ModelRevision != "v2" ||
		reports[0].Coverage() != 100 {
		t.Fatalf("after the switch: %+v", reports)
	}
}

// A worker never takes a job it cannot embed: the job stays pending for a
// process that serves its space. (Taking it and failing made it terminal;
// taking it and finishing hid the gap.)
func TestEmbeddingWorker_LeavesJobsOfOtherSpacesPending(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "nopipe.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Save(ctx, Memory{ID: "m", Category: "fact", Source: "test", Content: "some memory"}); err != nil {
		t.Fatal(err)
	}
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2"})
	other := EmbeddingIdentity{Provider: "bow", Model: "bow-multilingual", ModelRevision: "someone-else", Dimensions: 64, Normalization: "l2"}
	gen, err := store.EnsureEmbeddingGeneration(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureEmbeddingGenerationJobsOfKind(ctx, gen.ID, embeddingJobKind, 0); err != nil {
		t.Fatal(err)
	}
	drainUpgrade(t, svc)
	var state string
	var attempts int
	if err := store.DB().QueryRowContext(ctx, `SELECT state, attempts FROM memory_processing_jobs WHERE generation = ?`, gen.ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 0 {
		t.Fatalf("job of a foreign space: state=%s attempts=%d, want untouched", state, attempts)
	}
}

// secondProcess opens another store on the same database file, the way a
// second v0.20 process would: its own connection and its own vector cache.
func secondProcess(t *testing.T, store *SQLiteStore, embedder EmbeddingProvider) *Service {
	t.Helper()
	other, err := NewSQLiteStore(store.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	return newGenerationTestService(t, other, embedder)
}

func servedRevision(svc *Service) string {
	svc.searcher.generationMu.RLock()
	defer svc.searcher.generationMu.RUnlock()
	if svc.searcher.activeIdentity == nil {
		return ""
	}
	return svc.searcher.activeIdentity.ModelRevision
}

// Another process activating the v2 generation: this one notices at its next
// generation check and switches, dropping the legacy pipeline and the dual
// write.
func TestEmbeddingUpgrade_OtherProcessesFollowTheSwitch(t *testing.T) {
	ctx := context.Background()
	store, legacy, _ := legacyDatabase(t)
	a := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	b := secondProcess(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	if servedRevision(b) != "legacy" {
		t.Fatalf("b starts on the legacy generation, serves %q", servedRevision(b))
	}
	drainUpgrade(t, a)
	b.checkActiveGeneration(ctx)
	if servedRevision(b) != "v2" || b.legacyGenerationID() != "" {
		t.Fatalf("b after the check: serves %q, legacy loaded=%v", servedRevision(b), b.legacyGenerationID() != "")
	}
	for _, sp := range b.durableProcessingSpecs(false) {
		if sp.Kind != embeddingJobKindV2 {
			t.Fatalf("b still writes into the legacy generation: %+v", sp)
		}
	}
	if hits := vectorHits(t, b, "jwks jwt"); len(hits) == 0 || hits[0] != "auth" {
		t.Fatalf("b vector search after the switch: %v", hits)
	}
}

// An older binary reviving the legacy generation after the switch (it
// reactivates any complete generation of its own space): v0.20 follows it back
// to the legacy one, keeps search in one space, rebuilds v2 and switches again
// once the gates allow.
func TestEmbeddingUpgrade_SurvivesAnOlderBinaryRevivingTheLegacyGeneration(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	a := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	drainUpgrade(t, a)

	// What a v0.19 start does: retired -> building, then activate when full.
	legacyID, err := EmbeddingIdentityOf(legacy)
	if err != nil {
		t.Fatal(err)
	}
	old, err := NewSQLiteStore(store.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	// The older binary stays running: registered here since the test cannot
	// hold the file from another process.
	oldProc := ProcessInfo{PID: 4343, Host: "other-host", Version: "0.19.2", Role: "serve",
		StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC()}
	if err := old.RegisterProcess(ctx, oldProc); err != nil {
		t.Fatal(err)
	}
	if _, err := old.EnsureEmbeddingGeneration(ctx, legacyID); err != nil {
		t.Fatal(err)
	}
	if err := old.ActivateEmbeddingGeneration(ctx, legacyGen); err != nil {
		t.Fatal(err)
	}

	a.checkActiveGeneration(ctx)
	if servedRevision(a) != "legacy" || a.legacyGenerationID() != legacyGen {
		t.Fatalf("a must follow the revived legacy generation: serves %q", servedRevision(a))
	}
	if hits := vectorHits(t, a, "token bucket redis gateway"); len(hits) == 0 || hits[0] != "rate" {
		t.Fatalf("search on the revived legacy generation: %v", hits)
	}

	// The older binary exits: the next generation check retries the switch.
	if err := old.UnregisterProcess(ctx, oldProc.Host, oldProc.PID); err != nil {
		t.Fatal(err)
	}
	a.lastActivationTry = time.Time{}
	a.checkActiveGeneration(ctx)
	active, err := store.ActiveEmbeddingGeneration(ctx)
	if err != nil || active == nil || active.Identity.ModelRevision != "v2" {
		t.Fatalf("v2 must win again once the gates pass: %+v %v", active, err)
	}
}

// Only the current pipeline's generation is ever activated by v0.20.
func TestTryActivate_RefusesAGenerationThatIsNotTheCurrentOne(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	foreign := EmbeddingIdentity{Provider: "bow", Model: "bow-multilingual", ModelRevision: "v3", Dimensions: 64, Normalization: "l2"}
	gen, err := store.EnsureEmbeddingGeneration(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	revs, err := store.ListMissingEmbeddingRevisions(ctx, gen.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range revs {
		if err := store.PutEmbeddingVector(ctx, EmbeddingVectorRecord{RevisionID: r.RevisionID, MemoryID: r.MemoryID,
			GenerationID: gen.ID, Purpose: EmbeddingPurposeDocument, Identity: foreign,
			ContentHash: r.Memory.ContentHash, Vector: (&bowEmbedder{seed: 3}).vector(r.Memory.Content)}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := svc.tryActivateEmbeddingGeneration(ctx, store, gen.ID); ok || err != nil {
		t.Fatalf("activated a foreign generation: ok=%v err=%v", ok, err)
	}
	if active, _ := store.ActiveEmbeddingGeneration(ctx); active.ID != legacyGen {
		t.Fatalf("active generation changed to %s", active.ID)
	}
}

// Without /proc the gate cannot see binaries before v0.20 (they never
// register): it holds until the user confirms.
func TestEmbeddingUpgrade_HoldsWhereProcessesCannotBeSeen(t *testing.T) {
	ctx := context.Background()
	old := procRoot
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { procRoot = old })

	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	drainUpgrade(t, svc)
	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveGeneration != legacyGen || !strings.Contains(st.HeldBecause, "confirm_upgrade") {
		t.Fatalf("must hold without /proc: %+v", st)
	}
	svc.confirmEmbeddingUpgrade = true
	if ok, err := svc.tryActivateEmbeddingGeneration(ctx, store, st.BuildingGeneration); err != nil || !ok {
		t.Fatalf("confirm_upgrade must let it switch: ok=%v err=%v", ok, err)
	}
}

// Memories saved while no v0.20 process embedded them still get a vector in
// the legacy generation, so search stays complete during the whole build.
func TestEmbeddingUpgrade_ReconcilesTheLegacyGenerationItServes(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	if err := store.Save(ctx, Memory{ID: "late", Category: "fact", Source: "test", Content: "kafka partitions rebalance slowly"}); err != nil {
		t.Fatal(err)
	}
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	svc.holdEmbeddingUpgrade = true
	drainUpgrade(t, svc)
	if n, err := store.CountMissingEmbeddingRevisions(ctx, legacyGen); err != nil || n != 0 {
		t.Fatalf("legacy generation missing %d vectors (%v)", n, err)
	}
	if hits := vectorHits(t, svc, "kafka partitions rebalance"); len(hits) == 0 || hits[0] != "late" {
		t.Fatalf("the late memory must be found through the legacy generation: %v", hits)
	}
}

// A service whose main embedder is itself the legacy pipeline builds no v2
// generation and keeps the job kind older binaries share.
func TestEmbeddingUpgrade_LegacyEmbedderKeepsTheSharedJobKind(t *testing.T) {
	store, legacy, _ := legacyDatabase(t)
	svc := newGenerationTestService(t, store, legacy)
	if svc.legacyGenerationID() != "" || svc.runsV2Pipeline() {
		t.Fatal("a legacy embedder has no legacy pipeline of its own")
	}
	for _, sp := range svc.durableProcessingSpecs(false) {
		if sp.Kind != embeddingJobKind {
			t.Fatalf("spec %+v, want the shared kind", sp)
		}
	}
}

// The health sample and the report only count live memories.
func TestHealthSample_IgnoresDeletedMemories(t *testing.T) {
	ctx := context.Background()
	store, _, legacyGen := legacyDatabase(t)
	for _, id := range []string{"rate", "auth"} {
		if err := store.SoftDelete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	vecs, err := store.SampleEmbeddingVectors(ctx, legacyGen, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != len(upgradeCorpus)-2 {
		t.Fatalf("sampled %d vectors, want %d live ones", len(vecs), len(upgradeCorpus)-2)
	}
}

// Non-finite vectors cannot pass for a healthy space.
func TestEmbeddingUpgrade_UnmeasurableHealthHolds(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &nanEmbedder{bowEmbedder{seed: 2, revision: "v2", legacy: legacy}})
	drainUpgrade(t, svc)
	st, err := svc.EmbeddingUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.ActiveGeneration != legacyGen || !strings.Contains(st.HeldBecause, "could not be measured") {
		t.Fatalf("NaN vectors must hold the switch: %+v", st)
	}
}

type nanEmbedder struct{ bowEmbedder }

func (e *nanEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out, _ := e.bowEmbedder.Embed(ctx, texts)
	for _, v := range out {
		v[0] = float32(math.NaN())
	}
	return out, nil
}

// Searches running while the generation switches always get an answer
// scored in one space (run with -race).
func TestEmbeddingUpgrade_SearchDuringTheSwitch(t *testing.T) {
	store, legacy, _ := legacyDatabase(t)
	svc := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	done := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			res, err := svc.searcher.searchVector(context.Background(), "token bucket redis gateway", 5, nil, SearchOptions{})
			if err != nil {
				errs <- err
				return
			}
			if len(res) == 0 || res[0].Memory.ID != "rate" {
				errs <- fmt.Errorf("search %d returned %v", i, resultIDs(res))
				return
			}
		}
	}()
	drainUpgrade(t, svc)
	<-done
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

// A process still holding the legacy pipeline must not re-activate a legacy
// generation an older binary put back to building: switching is only ever
// towards the current pipeline's generation.
func TestTryActivate_NeverReactivatesTheLegacyGeneration(t *testing.T) {
	ctx := context.Background()
	store, legacy, legacyGen := legacyDatabase(t)
	a := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	b := secondProcess(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	drainUpgrade(t, a) // v2 active; b has not noticed and still holds the legacy pipeline
	legacyID, _ := EmbeddingIdentityOf(legacy)
	if _, err := store.EnsureEmbeddingGeneration(ctx, legacyID); err != nil { // retired -> building
		t.Fatal(err)
	}
	if ok, err := b.tryActivateEmbeddingGeneration(ctx, b.store.(*SQLiteStore), legacyGen); ok || err != nil {
		t.Fatalf("b re-activated the legacy generation: ok=%v err=%v", ok, err)
	}
	if active, _ := store.ActiveEmbeddingGeneration(ctx); active.Identity.ModelRevision != "v2" {
		t.Fatalf("active generation is %s", active.Identity.ModelRevision)
	}
}

// A vector of the active generation written by a process that still serves
// the legacy one must not enter that process's cache: its queries are
// embedded by the legacy pipeline.
func TestVectorCache_RefusesVectorsOfAnotherSpace(t *testing.T) {
	ctx := context.Background()
	store, legacy, _ := legacyDatabase(t)
	a := newGenerationTestService(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	b := secondProcess(t, store, &bowEmbedder{seed: 2, revision: "v2", legacy: legacy})
	drainUpgrade(t, a)
	bs := b.store.(*SQLiteStore)
	if err := bs.Save(ctx, Memory{ID: "fresh", Category: "fact", Source: "test", Content: "grpc deadlines propagate"}); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ActiveEmbeddingGeneration(ctx)
	revs, err := bs.ListMissingEmbeddingRevisions(ctx, active.ID, 10)
	if err != nil || len(revs) != 1 {
		t.Fatalf("missing in v2: %v %v", revs, err)
	}
	if err := bs.PutEmbeddingVector(ctx, EmbeddingVectorRecord{RevisionID: revs[0].RevisionID, MemoryID: "fresh",
		GenerationID: active.ID, Purpose: EmbeddingPurposeDocument, Identity: active.Identity,
		ContentHash: revs[0].Memory.ContentHash, Vector: (&bowEmbedder{seed: 2}).vector("grpc deadlines propagate")}); err != nil {
		t.Fatal(err)
	}
	if _, ok := bs.VectorCache().Get("fresh"); ok {
		t.Fatal("a v2 vector entered the cache of a process serving the legacy space")
	}
}

// The legacy pipeline has no legacy pipeline of its own.
func TestLegacyPipeline_OfTheLegacyPipelineFails(t *testing.T) {
	legacy := &ONNXEmbedder{legacy: true}
	if _, err := legacy.LegacyPipeline(); err == nil {
		t.Fatal("expected an error")
	}
}
