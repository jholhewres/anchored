package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/kg"
	"github.com/jholhewres/anchored/pkg/project"
)

type Service struct {
	store           Store
	searcher        *HybridSearcher
	sanitizer       *Sanitizer
	projDet         *project.Detector
	embedder        EmbeddingProvider
	cache           *EmbeddingCache
	logger          *slog.Logger
	embedSem        chan struct{}
	kgExtractor     *kg.PatternExtractor
	shutdown        chan struct{}
	wg              sync.WaitGroup
	observers       []MemoryObserver
	workerOnce      sync.Once
	workerWake      chan struct{}
	workerOwner     string
	closeOnce       sync.Once
	kgMu            sync.RWMutex
	deliveryMu      sync.RWMutex
	remoteDeliverer RemoteOutboxDeliverer
	embeddingMu     sync.RWMutex
	embeddingID     EmbeddingIdentity
	embeddingGenID  string
}

func NewService(cfg *config.Config, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}

	store, err := NewSQLiteStore(cfg.Memory.DatabasePath, logger)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	projDet := project.NewDetector(store.DB())

	sanitizer := NewSanitizer(cfg.Sanitizer)

	svc := &Service{
		store:     store,
		sanitizer: sanitizer,
		projDet:   projDet,
		logger:    logger,
		embedSem:  make(chan struct{}, 10),
		shutdown:  make(chan struct{}),
	}

	// provider "none" disables embeddings entirely (BM25-only search). It
	// must short-circuit BEFORE NewONNXEmbedder, which downloads the ONNX
	// runtime and model when ModelDir is empty — tests and minimal installs
	// rely on this to stay offline and fast.
	//
	// embedder is declared as the INTERFACE type on purpose: assigning a nil
	// *ONNXEmbedder would produce a non-nil interface, defeating the
	// `embedder == nil` guards in the searcher and panicking on first query.
	var embedder EmbeddingProvider
	if cfg.Embedding.Provider == "none" {
		logger.Info("embeddings disabled (provider: none), search will be BM25-only")
	} else {
		e, err := NewONNXEmbedder(cfg.Embedding.ModelDir, logger)
		if err != nil {
			logger.Warn("ONNX embedder not available, search will be BM25-only", "error", err)
		} else {
			embedder = e
			svc.embedder = e
		}
	}

	searchCfg := DefaultHybridSearchConfig()
	searchCfg.VectorWeight = cfg.Search.VectorWeight
	searchCfg.BM25Weight = cfg.Search.BM25Weight
	searchCfg.MaxResults = cfg.Search.MaxResults
	searchCfg.MMREnabled = cfg.Search.MMREnabled
	searchCfg.MMRLambda = cfg.Search.MMRLambda
	searchCfg.TemporalDecayEnabled = cfg.Search.TemporalDecayEnabled
	searchCfg.TemporalDecayHalfLifeDays = cfg.Search.TemporalDecayHalfLifeDays

	entityDetector := NewEntityDetector(store.DB(), DefaultEntityDetectorConfig(), logger)

	topicChangeDetector := NewTopicChangeDetector(embedder, entityDetector)

	svc.searcher = NewHybridSearcher(store, embedder, svc.cache, store.VectorCache(), searchCfg, entityDetector, topicChangeDetector, logger)
	if embedder != nil {
		if err := svc.ensureCurrentEmbeddingGeneration(context.Background()); err != nil {
			_ = embedder.Close()
			_ = store.Close()
			return nil, fmt.Errorf("initialize embedding generation: %w", err)
		}
	}
	svc.ensureDurableWorkers()

	return svc, nil
}

func (s *Service) Save(ctx context.Context, content, category, source string, cwd string) (*Memory, error) {
	return s.SaveWithOptions(ctx, SaveOptions{
		Content:  content,
		Category: category,
		Source:   source,
		CWD:      cwd,
	})
}

func (s *Service) SaveWithOptions(ctx context.Context, opts SaveOptions) (*Memory, error) {
	return s.saveWithOptions(ctx, opts, nil)
}

// SaveDurable atomically appends optional remote outbox envelopes while
// preserving the legacy SaveOptions API.
func (s *Service) SaveDurable(ctx context.Context, opts DurableSaveOptions) (*Memory, error) {
	return s.saveWithOptions(ctx, opts.SaveOptions, &opts)
}

func (s *Service) saveWithOptions(
	ctx context.Context,
	opts SaveOptions,
	durableOpts *DurableSaveOptions,
) (*Memory, error) {
	opts.Content = strings.TrimSpace(opts.Content)
	if opts.Content == "" {
		return nil, fmt.Errorf("content cannot be empty")
	}

	if s.sanitizer != nil {
		opts.Content = s.sanitizer.Sanitize(opts.Content)
	}

	if opts.Category == "" {
		opts.Category = Categorize(opts.Content)
	}

	var metadata any
	if opts.Metadata != nil {
		metadata = opts.Metadata
	} else {
		metadata = WithPreferenceScope(nil, opts.Category, opts.PreferenceScope)
	}

	var projectID *string
	if opts.CWD != "" {
		proj, err := s.projDet.Detect(opts.CWD)
		if err != nil {
			s.logger.Warn("project detection failed", "error", err)
		} else if proj != nil {
			projectID = &proj.ID
		}
	}
	hasProject := projectID != nil

	hash := contentHash(opts.Content)
	existing, err := s.store.FindByContentHash(ctx, hash, projectID)
	if err != nil {
		s.logger.Warn("content hash lookup failed, proceeding with save", "error", err)
	}
	// Exact-hash miss: look for a near-duplicate (a restatement of an existing
	// memory). Updating it in place keeps the store from accumulating
	// paraphrased duplicates that exact hashing can't catch.
	if existing == nil {
		existing = s.findNearDuplicate(ctx, opts.Content, projectID)
	}
	if existing != nil {
		metadata = WithPreferenceScope(existing.Metadata, opts.Category, opts.PreferenceScope)
		metadata = ApplyQualityMetadata(metadata, opts.Content, opts.Category, hasProject)
		upd := Memory{
			ID:          existing.ID,
			Content:     opts.Content,
			Category:    opts.Category,
			Source:      opts.Source,
			SourceID:    opts.SourceID,
			ProjectID:   projectID,
			ContentHash: hash,
			Keywords:    ExtractKeywords(opts.Content),
			CreatedAt:   existing.CreatedAt,
			Metadata:    metadata,
		}
		durable, err := s.persistSave(ctx, upd, opts, durableOpts)
		if err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		if durable {
			s.ensureDurableWorkers()
		}
		s.notifyObservers(func(obs MemoryObserver) {
			obs.OnMemoryUpdated(ctx, upd)
		})
		return &upd, nil
	}

	metadata = ApplyQualityMetadata(metadata, opts.Content, opts.Category, hasProject)
	m := Memory{
		// Assign the ID here rather than letting store.Save generate it: Save
		// takes Memory by value, so an ID generated there never propagates back
		// and embedAsync/observers would run with an empty ID — leaving the
		// embedding column unpopulated on the create path.
		ID:          newUUID(),
		Content:     opts.Content,
		Category:    opts.Category,
		Source:      opts.Source,
		SourceID:    opts.SourceID,
		ProjectID:   projectID,
		ContentHash: hash,
		Keywords:    ExtractKeywords(opts.Content),
		Metadata:    metadata,
	}

	durable, err := s.persistSave(ctx, m, opts, durableOpts)
	if err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}

	if durable {
		s.ensureDurableWorkers()
	} else if !opts.SkipEmbed {
		s.embedAsync(m.ID, m.Content)
	}

	s.kgMu.RLock()
	legacyKG := s.kgExtractor
	s.kgMu.RUnlock()
	if legacyKG != nil {
		s.wg.Add(1)
		go func(content string, projectID *string) {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := legacyKG.ExtractAndStore(ctx, content, projectID); err != nil {
				s.logger.Debug("kg extractor: error", "error", err)
			}
		}(m.Content, m.ProjectID)
	}

	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemorySaved(ctx, m)
	})

	return &m, nil
}

func (s *Service) persistSave(
	ctx context.Context,
	m Memory,
	opts SaveOptions,
	durableOpts *DurableSaveOptions,
) (bool, error) {
	var outbox []RemoteOutboxSpec
	if durableOpts != nil {
		outbox = append(outbox, durableOpts.RemoteOutbox...)
	}
	if durableOpts != nil && durableOpts.DeriveRemoteOutbox != nil {
		derived, err := durableOpts.DeriveRemoteOutbox(m)
		if err != nil {
			return false, fmt.Errorf("derive remote outbox: %w", err)
		}
		outbox = append(outbox, derived...)
	}

	temporal, temporalOK := s.store.(TemporalStore)
	_, queueOK := s.store.(ProcessingQueueStore)
	_, outboxOK := s.store.(RemoteOutboxStore)
	if temporalOK && (queueOK || len(outbox) > 0) {
		if len(outbox) > 0 && !outboxOK {
			return false, fmt.Errorf("store does not support durable remote outbox")
		}
		_, err := temporal.SaveTemporal(ctx, m, TemporalWriteOptions{
			ProcessingJobs: s.durableProcessingSpecs(opts.SkipEmbed),
			RemoteOutbox:   outbox,
		})
		return true, err
	}
	if len(outbox) > 0 {
		return false, fmt.Errorf("store does not support atomic remote outbox")
	}
	return false, s.store.Save(ctx, m)
}

func (s *Service) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}

	if s.searcher != nil {
		return s.searcher.Search(ctx, query, opts)
	}

	// BM25-only fallback (no hybrid searcher): expand the raw query into a safe
	// FTS expression so punctuation can't crash the match and multi-word queries
	// OR their tokens — mirroring HybridSearcher.searchBM25 so this path has the
	// same recall instead of FTS5's implicit-AND.
	fts := ExpandQueryAdvanced(query)
	if fts == "" {
		fts = ExpandQueryForFTS(ExtractKeywords(query))
	}
	if fts == "" {
		return nil, nil
	}
	return s.store.Search(ctx, fts, opts)
}

func (s *Service) Get(ctx context.Context, id string) (*Memory, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, opts ListOptions) ([]Memory, error) {
	return s.store.List(ctx, opts)
}

func (s *Service) Forget(ctx context.Context, id string) error {
	m, _ := s.store.Get(ctx, id)
	var pid *string
	if m != nil {
		pid = m.ProjectID
	}
	err := s.store.Delete(ctx, id)
	if err != nil {
		return err
	}
	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemoryDeleted(ctx, id, pid)
	})
	return nil
}

func (s *Service) Update(ctx context.Context, id, content, category string) (*Memory, error) {
	m, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get memory: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("memory %s not found", id)
	}

	if content != "" {
		content = strings.TrimSpace(content)
		if s.sanitizer != nil {
			content = s.sanitizer.Sanitize(content)
		}
	}
	if content == "" && category == "" {
		return nil, fmt.Errorf("must provide content or category to update")
	}

	updateContent := content
	if updateContent == "" {
		updateContent = m.Content
	}
	updateCategory := category
	if updateCategory == "" {
		updateCategory = m.Category
	}
	updatedMetadata := ApplyQualityMetadata(m.Metadata, updateContent, updateCategory, m.ProjectID != nil)
	_, durableQueue := s.store.(ProcessingQueueStore)
	writeOpts := TemporalWriteOptions{}
	if durableQueue {
		// Every temporal revision needs its own generation record even when
		// content is unchanged; otherwise an in-process legacy blob masks a
		// missing revision vector until restart.
		writeOpts.ProcessingJobs = s.durableProcessingSpecs(false)
	}

	if mutationStore, ok := s.store.(TemporalMutationStore); ok {
		mutation := TemporalMutation{
			Content:        &updateContent,
			Category:       &updateCategory,
			Metadata:       updatedMetadata,
			SetMetadata:    true,
			ClearEmbedding: content != "",
			ExpectedState:  TemporalStateActive,
		}
		if _, err := mutationStore.UpdateTemporal(ctx, id, mutation, writeOpts); err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
	} else if temporalStore, ok := s.store.(TemporalStore); ok {
		updated := *m
		updated.Content = updateContent
		updated.Category = updateCategory
		updated.ContentHash = contentHash(updateContent)
		updated.Keywords = ExtractKeywords(updateContent)
		updated.Metadata = updatedMetadata
		if content != "" {
			updated.Embedding = nil
		}
		if _, err := temporalStore.SaveTemporal(ctx, updated, writeOpts); err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
	} else {
		// Compatibility path for non-SQLite stores and existing test fakes that
		// do not advertise the optional temporal capability.
		if err := s.store.Update(ctx, id, updateContent, updateCategory); err != nil {
			return nil, fmt.Errorf("update: %w", err)
		}
		if err := s.store.UpdateMetadata(ctx, id, updatedMetadata); err != nil {
			return nil, fmt.Errorf("update metadata: %w", err)
		}
	}

	if durableQueue {
		s.ensureDurableWorkers()
	} else if content != "" {
		s.embedAsync(id, content)
	}

	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemoryUpdated(ctx, Memory{ID: id, Content: updateContent, Category: updateCategory, ProjectID: m.ProjectID})
	})

	m.Content = updateContent
	m.Category = updateCategory
	m.Metadata = updatedMetadata
	return m, nil
}

func (s *Service) UpdateMetadata(ctx context.Context, id string, metadata any) error {
	return s.store.UpdateMetadata(ctx, id, metadata)
}

func (s *Service) SoftForget(ctx context.Context, id string) error {
	m, _ := s.store.Get(ctx, id)
	var pid *string
	if m != nil {
		pid = m.ProjectID
	}
	err := s.store.SoftDelete(ctx, id)
	if err != nil {
		return err
	}
	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemoryDeleted(ctx, id, pid)
	})
	return nil
}

// Restore undoes a soft-delete so the memory re-enters search/list results.
// It mirrors SoftForget in reverse: project id is read directly (Get filters
// out deleted rows) so observers receive the same routing context they would
// for a save/update/delete, then the store clears deleted_at and the observer
// fan-out fires.
func (s *Service) Restore(ctx context.Context, id string) error {
	// Get excludes soft-deleted rows, so read the project id straight from the
	// store to keep observer routing accurate for (future) sync/KG listeners.
	var pid *string
	var projectID sql.NullString
	_ = s.store.DB().QueryRowContext(ctx,
		"SELECT project_id FROM memories WHERE id = ?", id).Scan(&projectID)
	if projectID.Valid && projectID.String != "" {
		v := projectID.String
		pid = &v
	}
	if _, durableQueue := s.store.(ProcessingQueueStore); durableQueue {
		if mutationStore, ok := s.store.(TemporalMutationStore); ok {
			if _, err := mutationStore.UpdateTemporal(ctx, id, TemporalMutation{
				ExpectedState: TemporalStateTombstone,
			}, TemporalWriteOptions{
				Mode:           TemporalSupersede,
				ProcessingJobs: s.durableProcessingSpecs(false),
			}); err != nil {
				return err
			}
			s.ensureDurableWorkers()
		} else if err := s.store.Restore(ctx, id); err != nil {
			return err
		}
	} else if err := s.store.Restore(ctx, id); err != nil {
		return err
	}
	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemoryRestored(ctx, id, pid)
	})
	return nil
}

// ForgetScope removes every memory matching the scope; with DryRun it only
// counts them.
func (s *Service) ForgetScope(ctx context.Context, opts DeleteScopeOptions) (int, error) {
	return s.store.DeleteByScope(ctx, opts)
}

func (s *Service) Stats(ctx context.Context) (*StoreStats, error) {
	return s.store.Stats(ctx)
}

func (s *Service) StoreDB() *sql.DB {
	return s.store.DB()
}

func (s *Service) VectorCache() *VectorCache {
	return s.store.VectorCache()
}

func (s *Service) BackfillContentHash(ctx context.Context) (int, error) {
	return s.store.BackfillContentHash(ctx)
}

// BackfillNormalizedHash stamps a bounded slice of pre-migration rows so they
// rejoin near-duplicate detection.
func (s *Service) BackfillNormalizedHash(ctx context.Context, limit int) (int, error) {
	return s.store.BackfillNormalizedHash(ctx, limit)
}

// PendingNormalizedHash reports how many rows still need stamping.
func (s *Service) PendingNormalizedHash(ctx context.Context) (int, error) {
	return s.store.PendingNormalizedHash(ctx)
}

// PendingEmbeddings reports how many memories are still missing a vector, so
// callers of a capped backfill run (`--max`) can report how much backlog
// remains.
func (s *Service) PendingEmbeddings(ctx context.Context) (int, error) {
	if generations, ok := s.store.(EmbeddingGenerationStore); ok {
		generationID := s.currentEmbeddingGenerationID()
		if generationID != "" {
			return generations.CountMissingEmbeddingRevisions(ctx, generationID)
		}
	}
	return s.store.CountWithoutEmbedding(ctx)
}

// ResolveProject returns the project ID for a given working directory, or empty string if none.
func (s *Service) ResolveProject(cwd string) string {
	if cwd == "" || s.projDet == nil {
		return ""
	}
	proj, err := s.projDet.Detect(cwd)
	if err != nil {
		s.logger.Debug("project detection failed", "cwd", cwd, "error", err)
		return ""
	}
	if proj == nil {
		return ""
	}
	return proj.ID
}

// ResolveProjectInfo returns the full project for a working directory (ID,
// Name and git-origin RemoteKey), or nil when cwd is not inside a git repo.
// Repo-scoped sync uses RemoteKey to identify a repository independent of its
// local directory name.
func (s *Service) ResolveProjectInfo(cwd string) (*project.Project, error) {
	if cwd == "" || s.projDet == nil {
		return nil, nil
	}
	return s.projDet.Detect(cwd)
}

func (s *Service) SaveMemory(ctx context.Context, content, category, source string, cwd string) error {
	_, err := s.SaveWithOptions(ctx, SaveOptions{
		Content:  content,
		Category: category,
		Source:   source,
		CWD:      cwd,
	})
	return err
}

func (s *Service) SaveRaw(ctx context.Context, content, category, source string, cwd string) error {
	_, err := s.SaveWithOptions(ctx, SaveOptions{
		Content:  content,
		Category: category,
		Source:   source,
		CWD:      cwd,
	})
	return err
}

func (s *Service) SaveRawNoEmbed(ctx context.Context, content, category, source string, cwd string) error {
	_, err := s.SaveWithOptions(ctx, SaveOptions{
		Content:   content,
		Category:  category,
		Source:    source,
		CWD:       cwd,
		SkipEmbed: true,
	})
	return err
}

func (s *Service) embedAsync(id, content string) {
	if s.embedder == nil || s.cache == nil {
		return
	}
	s.wg.Add(1)
	go func(id, content string) {
		defer s.wg.Done()
		select {
		case s.embedSem <- struct{}{}:
		case <-s.shutdown:
			return
		}
		defer func() { <-s.embedSem }()
		embedCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vecs, err := s.embedder.Embed(embedCtx, []string{content})
		if err != nil || len(vecs) == 0 {
			s.logger.Warn("embedding failed for cached memory", "error", err)
			return
		}
		if err := s.cache.Put(embedCtx, content, s.embedder.Model(), vecs[0], true); err != nil {
			s.logger.Warn("failed to cache embedding", "error", err)
		}
		if err := s.store.UpdateEmbedding(embedCtx, id, vecs[0]); err != nil {
			s.logger.Warn("failed to persist embedding to memory", "error", err)
		}
	}(id, content)
}

func (s *Service) SetKGExtractor(extractor *kg.PatternExtractor) {
	s.kgMu.Lock()
	s.kgExtractor = extractor
	s.kgMu.Unlock()
}

func (s *Service) RegisterObserver(obs MemoryObserver) {
	s.observers = append(s.observers, obs)
}

func (s *Service) notifyObservers(fn func(obs MemoryObserver)) {
	for _, obs := range s.observers {
		go func(o MemoryObserver) {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Debug("observer panic recovered", "error", r)
				}
			}()
			fn(o)
		}(obs)
	}
}

// EmbeddingsEnabled reports whether vector embedding is available (provider
// loaded + cache present). Callers use it to skip embedding-only background
// work (e.g. the serve-time backfill) instead of looping on a guaranteed error.
func (s *Service) EmbeddingsEnabled() bool {
	return s.embedder != nil
}

// BackfillEmbeddings embeds every memory still missing a vector, as fast as the
// machine allows (no pause between batches). Used by the one-shot `import`
// command where blocking-until-done is the desired behavior.
func (s *Service) BackfillEmbeddings(ctx context.Context, batchSize int) (int, error) {
	return s.BackfillEmbeddingsThrottled(ctx, batchSize, 0)
}

// BackfillEmbeddingsThrottled is the same backfill but sleeps `pause` between
// batches so a long historical backfill on a user's machine stays gentle on the
// CPU instead of pinning a core. It honors ctx cancellation between batches and
// between items, so a serve shutdown stops it promptly. Idempotent and
// resumable: it drains ListWithoutEmbedding, so re-running after an interrupted
// pass just continues where it left off.
func (s *Service) BackfillEmbeddingsThrottled(ctx context.Context, batchSize int, pause time.Duration) (int, error) {
	return s.BackfillEmbeddingsLimited(ctx, batchSize, pause, 0)
}

// BackfillEmbeddingsLimited is BackfillEmbeddingsThrottled bounded by maxTotal
// memories embedded during this call (0 or negative = unlimited, same as
// BackfillEmbeddingsThrottled). Used by `anchored backfill --max` and the
// maintenance "backfill" step so a daily timer chews the backlog in bounded
// slices instead of one multi-hour run.
func (s *Service) BackfillEmbeddingsLimited(ctx context.Context, batchSize int, pause time.Duration, maxTotal int) (int, error) {
	if s.embedder == nil {
		return 0, fmt.Errorf("embedder not available")
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	if generations, ok := s.store.(EmbeddingGenerationStore); ok {
		return s.backfillEmbeddingGeneration(ctx, generations, batchSize, pause, maxTotal)
	}
	if s.cache == nil {
		return 0, fmt.Errorf("embedding cache not available")
	}

	var total int
	var lastTotal int
	stuckCount := 0
	const maxStuck = 3

	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		if maxTotal > 0 && total >= maxTotal {
			break
		}

		listLimit := batchSize
		if maxTotal > 0 && maxTotal-total < listLimit {
			listLimit = maxTotal - total
		}

		mems, err := s.store.ListWithoutEmbedding(ctx, listLimit)
		if err != nil {
			return total, fmt.Errorf("list pending: %w", err)
		}
		if len(mems) == 0 {
			break
		}

		batchOK := 0
		for _, m := range mems {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			vecs, err := s.embedder.Embed(ctx, []string{m.Content})
			if err != nil || len(vecs) == 0 {
				s.logger.Warn("backfill embedding failed", "id", m.ID, "error", err)
				continue
			}
			vec := vecs[0]
			if err := s.cache.Put(ctx, m.Content, s.embedder.Model(), vec, true); err != nil {
				s.logger.Warn("backfill cache failed", "id", m.ID, "error", err)
			}
			if err := s.store.UpdateEmbedding(ctx, m.ID, vec); err != nil {
				s.logger.Warn("backfill persist failed", "id", m.ID, "error", err)
				continue
			}
			batchOK++
			total++
		}

		s.logger.Info("backfill embeddings", "batch", len(mems), "succeeded", batchOK, "total", total)

		if total == lastTotal {
			stuckCount++
			if stuckCount >= maxStuck {
				s.logger.Warn("backfill stuck: no progress after consecutive batches, stopping", "attempts", stuckCount)
				break
			}
		} else {
			stuckCount = 0
		}
		lastTotal = total

		if pause > 0 {
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(pause):
			}
		}
	}

	return total, nil
}

func (s *Service) Close() {
	s.closeOnce.Do(func() {
		if s.shutdown != nil {
			close(s.shutdown)
		}
		s.wg.Wait()
		if s.embedder != nil {
			s.embedder.Close()
		}
		if s.store != nil {
			s.store.Close()
		}
	})
}

// normalizeForDedup folds the trivial variations exact hashing misses:
// case and whitespace. It deliberately does NOT tokenize or measure fuzzy
// similarity — two memories merge only when they are the SAME text modulo
// case/spacing, never when they merely share vocabulary (e.g. "deployed v1"
// and "deployed v2", or "...decision A..." and "...decision B...", stay
// distinct).
func normalizeForDedup(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// findNearDuplicate returns an existing memory whose content is identical to
// `content` after case/whitespace normalization, within the same project, or
// nil. It fetches a few BM25 candidates so the check stays cheap (no
// synchronous embedding). This catches "Postgres" vs "postgres " without the
// false-merge risk of fuzzy similarity.
// findNearDuplicate returns an existing memory whose content is identical after
// lowercasing and collapsing whitespace — a restatement that exact hashing
// misses. It is an indexed equality lookup, not a search: the predicate has
// always been exact normalized equality, and generating candidates with a
// full-text query over every keyword in the content cost seconds per save on a
// large store (3.6s average, 16s worst case at 3.3GB) for a question an index
// answers outright.
//
// Rows predating the normalized_hash column read as a miss until `anchored
// backfill` stamps them, which costs a duplicate row that dream reclaims —
// never a wrong match.
func (s *Service) findNearDuplicate(ctx context.Context, content string, projectID *string) *Memory {
	existing, err := s.store.FindByNormalizedHash(ctx, normalizedHash(content), projectID)
	if err != nil {
		s.logger.Warn("normalized hash lookup failed, proceeding with save", "error", err)
		return nil
	}
	return existing
}
