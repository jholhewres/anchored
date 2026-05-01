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
	store        Store
	searcher     *HybridSearcher
	sanitizer    *Sanitizer
	projDet      *project.Detector
	embedder     EmbeddingProvider
	cache        *EmbeddingCache
	logger       *slog.Logger
	embedSem     chan struct{}
	kgExtractor  *kg.PatternExtractor
	shutdown     chan struct{}
	wg           sync.WaitGroup
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

	sanitizer := NewSanitizer(cfg.Sanitizer.Enabled)

	svc := &Service{
		store:     store,
		sanitizer: sanitizer,
		projDet:   projDet,
		logger:    logger,
		embedSem:  make(chan struct{}, 10),
		shutdown:  make(chan struct{}),
	}

	embedder, err := NewONNXEmbedder(cfg.Embedding.ModelDir, logger)
	if err != nil {
		logger.Warn("ONNX embedder not available, search will be BM25-only", "error", err)
	} else {
		svc.embedder = embedder
		svc.cache = NewEmbeddingCache(store.DB(), logger)
		svc.cache.MigrateFromLegacy(embedder.Model())
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

	var projectID *string
	if opts.CWD != "" {
		proj, err := s.projDet.Detect(opts.CWD)
		if err != nil {
			s.logger.Warn("project detection failed", "error", err)
		} else if proj != nil {
			projectID = &proj.ID
		}
	}

	hash := contentHash(opts.Content)
	existing, err := s.store.FindByContentHash(ctx, hash, projectID)
	if err != nil {
		s.logger.Warn("content hash lookup failed, proceeding with save", "error", err)
	} else if existing != nil {
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
		}
		if err := s.store.Save(ctx, upd); err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		return &upd, nil
	}

	m := Memory{
		Content:     opts.Content,
		Category:    opts.Category,
		Source:      opts.Source,
		SourceID:    opts.SourceID,
		ProjectID:   projectID,
		ContentHash: hash,
		Keywords:    ExtractKeywords(opts.Content),
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

	return s.store.Search(ctx, query, opts)
}

func (s *Service) Get(ctx context.Context, id string) (*Memory, error) {
	return s.store.Get(ctx, id)
}

func (s *Service) List(ctx context.Context, opts ListOptions) ([]Memory, error) {
	return s.store.List(ctx, opts)
}

func (s *Service) Forget(ctx context.Context, id string) error {
	return s.store.Delete(ctx, id)
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

	if err := s.store.Update(ctx, id, updateContent, updateCategory); err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}

	if content != "" {
		s.embedAsync(id, content)
	}

	m.Content = updateContent
	m.Category = updateCategory
	return m, nil
}

func (s *Service) SoftForget(ctx context.Context, id string) error {
	return s.store.SoftDelete(ctx, id)
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

func (s *Service) BackfillEmbeddings(ctx context.Context, batchSize int) (int, error) {
	if s.embedder == nil || s.cache == nil {
		return 0, fmt.Errorf("embedder not available")
	}
	if batchSize <= 0 {
		batchSize = 100
	}

	var total int
	for {
		mems, err := s.store.ListWithoutEmbedding(ctx, batchSize)
		if err != nil {
			return total, fmt.Errorf("list pending: %w", err)
		}
		if len(mems) == 0 {
			break
		}

		for _, m := range mems {
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
			total++
		}

		s.logger.Info("backfill embeddings", "batch", len(mems), "total", total)
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
