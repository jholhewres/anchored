package memory

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/project"
)

type Service struct {
	store     Store
	searcher  *HybridSearcher
	sanitizer *Sanitizer
	projDet   *project.Detector
	embedder  EmbeddingProvider
	cache     *EmbeddingCache
	logger    *slog.Logger
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
	}

	embedder, err := NewONNXEmbedder(cfg.Embedding.ModelDir, logger)
	if err != nil {
		logger.Warn("ONNX embedder not available, search will be BM25-only", "error", err)
	} else {
		svc.embedder = embedder
		svc.cache = NewEmbeddingCache(store.DB(), logger)
	}

	searchCfg := DefaultHybridSearchConfig()
	searchCfg.VectorWeight = cfg.Search.VectorWeight
	searchCfg.BM25Weight = cfg.Search.BM25Weight
	searchCfg.MaxResults = cfg.Search.MaxResults
	searchCfg.MMREnabled = cfg.Search.MMREnabled
	searchCfg.MMRLambda = cfg.Search.MMRLambda
	searchCfg.TemporalDecayEnabled = cfg.Search.TemporalDecayEnabled
	searchCfg.TemporalDecayHalfLifeDays = cfg.Search.TemporalDecayHalfLifeDays

	svc.searcher = NewHybridSearcher(store, embedder, svc.cache, searchCfg, logger)

	return svc, nil
}

func (s *Service) Save(ctx context.Context, content, category, source string, cwd string) (*Memory, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("content cannot be empty")
	}

	if s.sanitizer != nil {
		content = s.sanitizer.Sanitize(content)
	}

	if category == "" {
		category = Categorize(content)
	}

	var projectID *string
	if cwd != "" {
		proj, err := s.projDet.Detect(cwd)
		if err != nil {
			s.logger.Warn("project detection failed", "error", err)
		} else if proj != nil {
			projectID = &proj.ID
		}
	}

	m := Memory{
		Content:   content,
		Category:  category,
		Source:    source,
		ProjectID: projectID,
		Keywords:  ExtractKeywords(content),
	}

	if err := s.store.Save(ctx, m); err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}

	if s.embedder != nil && s.cache != nil {
		go func(content string) {
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
		}(m.Content)
	}

	return &m, nil
}

func (s *Service) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query cannot be empty")
	}

	if s.searcher != nil {
		return s.searcher.Search(ctx, query)
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

func (s *Service) Stats(ctx context.Context) (*StoreStats, error) {
	return s.store.Stats(ctx)
}

func (s *Service) StoreDB() *sql.DB {
	return s.store.DB()
}

func (s *Service) SaveMemory(ctx context.Context, content, category, source string, cwd string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	if s.sanitizer != nil {
		content = s.sanitizer.Sanitize(content)
	}

	if category == "" {
		category = Categorize(content)
	}

	var projectID *string
	if cwd != "" {
		proj, err := s.projDet.Detect(cwd)
		if err != nil {
			s.logger.Warn("project detection failed", "error", err)
		} else if proj != nil {
			projectID = &proj.ID
		}
	}

	m := Memory{
		Content:   content,
		Category:  category,
		Source:    source,
		ProjectID: projectID,
		Keywords:  ExtractKeywords(content),
	}

	if err := s.store.Save(ctx, m); err != nil {
		return fmt.Errorf("save: %w", err)
	}

	s.embedAsync(m.Content)

	return nil
}

func (s *Service) SaveRaw(ctx context.Context, content, category, source string, cwd string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	if s.sanitizer != nil {
		content = s.sanitizer.Sanitize(content)
	}

	if category == "" {
		category = Categorize(content)
	}

	var projectID *string
	if cwd != "" {
		proj, err := s.projDet.Detect(cwd)
		if err != nil {
			s.logger.Warn("project detection failed", "error", err)
		} else if proj != nil {
			projectID = &proj.ID
		}
	}

	m := Memory{
		Content:   content,
		Category:  category,
		Source:    source,
		ProjectID: projectID,
		Keywords:  ExtractKeywords(content),
	}

	return s.store.Save(ctx, m)
}

func (s *Service) embedAsync(content string) {
	if s.embedder == nil || s.cache == nil {
		return
	}
	go func(content string) {
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
	}(content)
}

func (s *Service) Close() {
	if s.embedder != nil {
		s.embedder.Close()
	}
	if s.store != nil {
		s.store.Close()
	}
}
