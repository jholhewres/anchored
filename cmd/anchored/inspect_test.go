package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jholhewres/anchored/pkg/config"
)

func writeTestConfig(t *testing.T, path string, cfg *config.Config) {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func newTestEnv(t *testing.T) string {
	t.Helper()
	return newTestEnvWith(t, nil)
}

// newTestEnvWith writes a config with its own database, changed by mutate
// when it is not nil, and returns the config path.
func newTestEnvWith(t *testing.T, mutate func(*config.Config)) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfg := config.Defaults()
	cfg.Memory.DatabasePath = dbPath
	cfg.Embedding.Provider = "none"
	cfg.Embedding.ModelDir = filepath.Join(dir, "onnx")
	if mutate != nil {
		mutate(cfg)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	writeTestConfig(t, cfgPath, cfg)
	return cfgPath
}

func TestInspectOutput(t *testing.T) {
	cfgPath := newTestEnv(t)

	_, _, svc, err := initService(cfgPath)
	if err != nil {
		t.Fatalf("initService: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	_, err = svc.Save(ctx, "test content for inspect", "fact", "test", "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	var id string
	if err := svc.StoreDB().QueryRowContext(ctx,
		"SELECT id FROM memories WHERE content = ? ORDER BY created_at DESC LIMIT 1",
		"test content for inspect",
	).Scan(&id); err != nil {
		t.Fatalf("lookup saved memory: %v", err)
	}

	m, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if m == nil {
		t.Fatal("Get returned nil for existing memory")
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if parsed["id"] != id {
		t.Errorf("parsed id = %v, want %s", parsed["id"], id)
	}
	if parsed["content"] != "test content for inspect" {
		t.Errorf("parsed content = %v, want 'test content for inspect'", parsed["content"])
	}
	if parsed["category"] != "fact" {
		t.Errorf("parsed category = %v, want 'fact'", parsed["category"])
	}
	if parsed["source"] != "test" {
		t.Errorf("parsed source = %v, want 'test'", parsed["source"])
	}
}

func TestInspectDataPersistsAcrossRestart(t *testing.T) {
	cfgPath := newTestEnv(t)

	_, _, svc, err := initService(cfgPath)
	if err != nil {
		t.Fatalf("initService: %v", err)
	}

	ctx := context.Background()
	_, err = svc.Save(ctx, "persistence test", "decision", "test", "")
	if err != nil {
		svc.Close()
		t.Fatalf("Save: %v", err)
	}

	var id string
	if err := svc.StoreDB().QueryRowContext(ctx,
		"SELECT id FROM memories WHERE content = ? ORDER BY created_at DESC LIMIT 1",
		"persistence test",
	).Scan(&id); err != nil {
		svc.Close()
		t.Fatalf("lookup: %v", err)
	}
	svc.Close()

	_, _, svc2, err := initService(cfgPath)
	if err != nil {
		t.Fatalf("second initService: %v", err)
	}
	m2, err := svc2.Get(ctx, id)
	svc2.Close()
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if m2 == nil {
		t.Fatalf("second Get returned nil for id=%s", id)
	}
	if m2.Content != "persistence test" {
		t.Errorf("content = %q, want 'persistence test'", m2.Content)
	}
}

