package stack

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultBudget = 3600

type StackMetrics struct {
	LayerBytesL0  int64
	LayerBytesL1  int64
	LayerBytesL2  int64
	L1CacheHits   int64
	L1CacheMisses int64
	TotalRenders  int64
}

type LayerOutput struct {
	Label   string
	Content string
	Bytes   int
}

type Stack struct {
	identity     *IdentityLayer
	project      *ProjectLayer
	ondemand     *OnDemandLayer
	budget       int
	logger       *slog.Logger
	layerBytesL0 atomic.Int64
	layerBytesL1 atomic.Int64
	layerBytesL2 atomic.Int64
	totalRenders atomic.Int64
}

func NewStack(identity *IdentityLayer, project *ProjectLayer, ondemand *OnDemandLayer, budget int, logger *slog.Logger) *Stack {
	if budget <= 0 {
		budget = defaultBudget
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Stack{
		identity: identity,
		project:  project,
		ondemand: ondemand,
		budget:   budget,
		logger:   logger,
	}
}

const (
	layerPriorityIdentity  = 0
	layerPriorityProject   = 1
	layerPriorityOnDemand  = 2
)

func (s *Stack) Render() string {
	s.totalRenders.Add(1)

	type indexedLayer struct {
		output   LayerOutput
		priority int
	}

	var layers []indexedLayer
	var bytesL0, bytesL1, bytesL2 int

	l0 := s.identity.Render()
	if l0 != "" {
		layers = append(layers, indexedLayer{LayerOutput{Label: "identity", Content: l0, Bytes: len(l0)}, layerPriorityIdentity})
		bytesL0 = len(l0)
	}
	s.layerBytesL0.Store(int64(bytesL0))

	if s.project != nil {
		l1 := s.project.Render()
		if l1 != "" {
			layers = append(layers, indexedLayer{LayerOutput{Label: "project", Content: l1, Bytes: len(l1)}, layerPriorityProject})
			bytesL1 = len(l1)
		}
	}
	s.layerBytesL1.Store(int64(bytesL1))

	if s.ondemand != nil {
		l2 := s.ondemand.Render("", "")
		if l2 != "" {
			layers = append(layers, indexedLayer{LayerOutput{Label: "ondemand", Content: l2, Bytes: len(l2)}, layerPriorityOnDemand})
			bytesL2 = len(l2)
		}
	}
	s.layerBytesL2.Store(int64(bytesL2))

	sort.Slice(layers, func(i, j int) bool {
		return layers[i].priority < layers[j].priority
	})

	var budgetUsed int
	var parts []string

	for _, il := range layers {
		l := il.output
		if budgetUsed+len(l.Content) > s.budget {
			remaining := s.budget - budgetUsed
			if remaining <= 10 {
				break
			}
			l.Content = truncateToBytes(l.Content, remaining)
			parts = append(parts, l.Content)
			budgetUsed += len(l.Content)
			break
		}
		parts = append(parts, l.Content)
		budgetUsed += len(l.Content)
	}

	if len(parts) == 0 {
		return ""
	}

	s.logger.Debug("stack render", "l0", bytesL0, "l1", bytesL1, "l2", bytesL2, "total", bytesL0+bytesL1+bytesL2)

	return fmt.Sprintf("## Anchored Memory\n\n%s", strings.Join(parts, "\n\n"))
}

func (s *Stack) Metrics() StackMetrics {
	m := StackMetrics{
		LayerBytesL0: s.layerBytesL0.Load(),
		LayerBytesL1: s.layerBytesL1.Load(),
		LayerBytesL2: s.layerBytesL2.Load(),
		TotalRenders: s.totalRenders.Load(),
	}
	if s.project != nil && s.project.essential != nil {
		m.L1CacheHits = s.project.essential.CacheHits()
		m.L1CacheMisses = s.project.essential.CacheMisses()
	}
	return m
}

func truncateToBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}

	s = s[:maxBytes]
	if i := strings.LastIndex(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

type IdentityLayer struct {
	path    string
	logger  *slog.Logger
	budget  int
	content string
	modTime time.Time
	done    chan struct{}
	mu      sync.Mutex
	started bool
	stopped bool
}

func NewIdentityLayer(path string, logger *slog.Logger, budget int) *IdentityLayer {
	if logger == nil {
		logger = slog.Default()
	}
	if budget <= 0 {
		budget = 800
	}
	return &IdentityLayer{path: path, logger: logger, budget: budget, done: make(chan struct{})}
}

func (l *IdentityLayer) Start() error {
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return nil
	}
	l.started = true
	l.mu.Unlock()

	if err := l.reload(); err != nil {
		l.logger.Warn("identity: initial load failed", "error", err)
	}
	go l.pollLoop()
	return nil
}

func (l *IdentityLayer) Stop() {
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return
	}
	l.stopped = true
	l.mu.Unlock()

	close(l.done)
}

func (l *IdentityLayer) Render() string {
	if len(l.content) > l.budget {
		return l.content[:l.budget]
	}
	return l.content
}

func (l *IdentityLayer) Reload() error { return l.reload() }

func (l *IdentityLayer) reload() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			l.content = ""
			return nil
		}
		return err
	}

	info, statErr := os.Stat(l.path)
	var modTime time.Time
	if statErr == nil {
		modTime = info.ModTime()
	}

	l.content = string(data)
	l.modTime = modTime
	return nil
}

func (l *IdentityLayer) pollLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			info, err := os.Stat(l.path)
			if err != nil {
				continue
			}
			if info.ModTime().After(l.modTime) {
				_ = l.reload()
			}
		}
	}
}

type ProjectLayer struct {
	essential  *EssentialLayer
	projectID  string
	getStoryFn func() string
}

func NewProjectLayer(getStoryFn func() string) *ProjectLayer {
	return &ProjectLayer{getStoryFn: getStoryFn}
}

func NewProjectLayerWithEssential(store DBAccessor, projectID string, logger *slog.Logger) *ProjectLayer {
	return &ProjectLayer{
		essential: NewEssentialLayer(store, logger),
		projectID: projectID,
	}
}

func (l *ProjectLayer) Render() string {
	if l.essential != nil {
		return l.essential.Render(l.projectID)
	}
	if l.getStoryFn != nil {
		return l.getStoryFn()
	}
	return ""
}

func (l *ProjectLayer) Invalidate() {
	if l.essential != nil {
		l.essential.Invalidate(l.projectID)
	}
}


