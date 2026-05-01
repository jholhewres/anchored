package stack

import (
	"testing"
)

func TestStack_Metrics_TotalRenders(t *testing.T) {
	identity := NewIdentityLayer("", nil, 800)
	stack := NewStack(identity, nil, nil, defaultBudget, nil)

	m := stack.Metrics()
	if m.TotalRenders != 0 {
		t.Fatalf("expected 0 renders before any render, got %d", m.TotalRenders)
	}

	stack.Render()
	stack.Render()
	stack.Render()

	m = stack.Metrics()
	if m.TotalRenders != 3 {
		t.Errorf("expected 3 renders, got %d", m.TotalRenders)
	}
}

func TestStack_Metrics_LayerBytes(t *testing.T) {
	identity := NewIdentityLayer("", nil, 800)
	project := NewProjectLayer(func() string { return "project content here" })
	stack := NewStack(identity, project, nil, defaultBudget, nil)

	stack.Render()

	m := stack.Metrics()
	if m.TotalRenders != 1 {
		t.Fatalf("expected 1 render, got %d", m.TotalRenders)
	}
	if m.LayerBytesL1 != 20 {
		t.Errorf("expected L1 bytes=20, got %d", m.LayerBytesL1)
	}
}

func TestStack_Metrics_L1CacheHitMiss(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()
	insertMemory(t, db, "proj1", "fact", "Test fact", 5)

	essential := NewEssentialLayer(accessor, nil)
	project := &ProjectLayer{essential: essential, projectID: "proj1"}
	identity := NewIdentityLayer("", nil, 800)
	stack := NewStack(identity, project, nil, defaultBudget, nil)

	stack.Render()
	m := stack.Metrics()
	if m.L1CacheMisses != 1 {
		t.Errorf("expected 1 cache miss after first render, got %d", m.L1CacheMisses)
	}
	if m.L1CacheHits != 0 {
		t.Errorf("expected 0 cache hits after first render, got %d", m.L1CacheHits)
	}

	stack.Render()
	m = stack.Metrics()
	if m.L1CacheHits != 1 {
		t.Errorf("expected 1 cache hit after second render, got %d", m.L1CacheHits)
	}
	if m.L1CacheMisses != 1 {
		t.Errorf("expected cache misses to stay at 1, got %d", m.L1CacheMisses)
	}
}

func TestEssentialLayer_CacheHitMissCounters(t *testing.T) {
	accessor := setupTestDB(t)
	db := accessor.DB()
	insertMemory(t, db, "p1", "fact", "A fact", 3)

	layer := NewEssentialLayer(accessor, nil)

	if layer.CacheHits() != 0 || layer.CacheMisses() != 0 {
		t.Fatal("counters should start at zero")
	}

	layer.Render("p1")
	if layer.CacheMisses() != 1 {
		t.Errorf("expected 1 miss, got %d", layer.CacheMisses())
	}
	if layer.CacheHits() != 0 {
		t.Errorf("expected 0 hits, got %d", layer.CacheHits())
	}

	layer.Render("p1")
	if layer.CacheHits() != 1 {
		t.Errorf("expected 1 hit on second call, got %d", layer.CacheHits())
	}
	if layer.CacheMisses() != 1 {
		t.Errorf("misses should stay 1, got %d", layer.CacheMisses())
	}
}

func TestStack_Metrics_NilLayers(t *testing.T) {
	identity := NewIdentityLayer("", nil, 800)
	stack := NewStack(identity, nil, nil, defaultBudget, nil)

	stack.Render()

	m := stack.Metrics()
	if m.TotalRenders != 1 {
		t.Errorf("expected 1 render, got %d", m.TotalRenders)
	}
	if m.LayerBytesL1 != 0 {
		t.Errorf("expected 0 L1 bytes with nil project, got %d", m.LayerBytesL1)
	}
	if m.LayerBytesL2 != 0 {
		t.Errorf("expected 0 L2 bytes with nil ondemand, got %d", m.LayerBytesL2)
	}
	if m.L1CacheHits != 0 || m.L1CacheMisses != 0 {
		t.Error("expected 0 L1 cache stats with nil project")
	}
}
