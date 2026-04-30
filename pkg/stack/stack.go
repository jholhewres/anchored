package stack

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
)

const defaultBudget = 3600

type LayerOutput struct {
	Label   string
	Content string
	Bytes   int
}

type Stack struct {
	identity  *IdentityLayer
	project   *ProjectLayer
	ondemand  *OnDemandLayer
	budget    int
	logger    *slog.Logger
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
	type indexedLayer struct {
		output  LayerOutput
		priority int
	}

	var layers []indexedLayer

	l0 := s.identity.Render()
	if l0 != "" {
		layers = append(layers, indexedLayer{LayerOutput{Label: "identity", Content: l0, Bytes: len(l0)}, layerPriorityIdentity})
	}

	if s.project != nil {
		l1 := s.project.Render()
		if l1 != "" {
			layers = append(layers, indexedLayer{LayerOutput{Label: "project", Content: l1, Bytes: len(l1)}, layerPriorityProject})
		}
	}

	if s.ondemand != nil {
		l2 := s.ondemand.Render()
		if l2 != "" {
			layers = append(layers, indexedLayer{LayerOutput{Label: "ondemand", Content: l2, Bytes: len(l2)}, layerPriorityOnDemand})
		}
	}

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

	return fmt.Sprintf("## Anchored Memory\n\n%s", strings.Join(parts, "\n\n"))
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
	if err := l.reload(); err != nil {
		l.logger.Warn("identity: initial load failed", "error", err)
	}
	go l.pollLoop()
	return nil
}

func (l *IdentityLayer) Stop() {
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
	getStoryFn func() string
	stale     time.Duration
}

func NewProjectLayer(getStoryFn func() string, stale time.Duration) *ProjectLayer {
	if stale <= 0 {
		stale = 6 * time.Hour
	}
	return &ProjectLayer{getStoryFn: getStoryFn, stale: stale}
}

func (l *ProjectLayer) Render() string {
	if l.getStoryFn == nil {
		return ""
	}
	return l.getStoryFn()
}

type OnDemandLayer struct {
	getRelevantFn func() string
}

func NewOnDemandLayer(getRelevantFn func() string) *OnDemandLayer {
	return &OnDemandLayer{getRelevantFn: getRelevantFn}
}

func (l *OnDemandLayer) Render() string {
	if l.getRelevantFn == nil {
		return ""
	}
	return l.getRelevantFn()
}
