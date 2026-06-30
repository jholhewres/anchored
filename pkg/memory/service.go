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
	store       Store
	searcher    *HybridSearcher
	sanitizer   *Sanitizer
	projDet     *project.Detector
	embedder    EmbeddingProvider
	cache       *EmbeddingCache
	logger      *slog.Logger
	embedSem    chan struct{}
	kgExtractor *kg.PatternExtractor
	shutdown    chan struct{}
	wg          sync.WaitGroup
	observers   []MemoryObserver
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
			svc.cache = NewEmbeddingCache(store.DB(), logger)
			svc.cache.MigrateFromLegacy(e.Model())
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
		if err := s.store.Save(ctx, upd); err != nil {
			return nil, fmt.Errorf("save: %w", err)
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

	if err := s.store.Save(ctx, m); err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}

	if !opts.SkipEmbed {
		s.embedAsync(m.ID, m.Content)
	}

	if s.kgExtractor != nil {
		s.wg.Add(1)
		go func(content string, projectID *string) {
			defer s.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.kgExtractor.ExtractAndStore(ctx, content, projectID); err != nil {
				s.logger.Debug("kg extractor: error", "error", err)
			}
		}(m.Content, m.ProjectID)
	}

	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemorySaved(ctx, m)
	})

	return &m, nil
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

	if err := s.store.Update(ctx, id, updateContent, updateCategory); err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	if err := s.store.UpdateMetadata(ctx, id, updatedMetadata); err != nil {
		return nil, fmt.Errorf("update metadata: %w", err)
	}

	if content != "" {
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
	if err := s.store.Restore(ctx, id); err != nil {
		return err
	}
	s.notifyObservers(func(obs MemoryObserver) {
		obs.OnMemoryRestored(ctx, id, pid)
	})
	return nil
}

func (s *Service) ForgetByScope(ctx context.Context, projectID, category, source string, hard bool) (int, error) {
	return s.store.DeleteByScope(ctx, DeleteScopeOptions{
		ProjectID: projectID,
		Category:  category,
		Source:    source,
		Hard:      hard,
	})
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
	s.kgExtractor = extractor
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
	return s.embedder != nil && s.cache != nil
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
	if s.embedder == nil || s.cache == nil {
		return 0, fmt.Errorf("embedder not available")
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	var total int
	var lastTotal int
	stuckCount := 0
	const maxStuck = 3

	for {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}

		mems, err := s.store.ListWithoutEmbedding(ctx, batchSize)
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
	close(s.shutdown)
	s.wg.Wait()
	if s.embedder != nil {
		s.embedder.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
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
func (s *Service) findNearDuplicate(ctx context.Context, content string, projectID *string) *Memory {
	kws := ExtractKeywords(content)
	if len(kws) == 0 {
		return nil
	}
	fts := ExpandQueryForFTS(kws)
	if fts == "" {
		return nil
	}
	opts := SearchOptions{MaxResults: 10}
	if projectID != nil {
		opts.ProjectID = *projectID
	}
	cands, err := s.store.Search(ctx, fts, opts)
	if err != nil {
		return nil
	}
	want := normalizeForDedup(content)
	for i := range cands {
		if normalizeForDedup(cands[i].Memory.Content) == want {
			m := cands[i].Memory
			return &m
		}
	}
	return nil
}
