package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Memory    MemoryConfig    `yaml:"memory"`
	Embedding EmbeddingConfig `yaml:"embedding"`
	Search    SearchConfig    `yaml:"search"`
	Sanitizer SanitizerConfig `yaml:"sanitizer"`
	Stack     StackConfig     `yaml:"stack"`
}

type MemoryConfig struct {
	StorageDir   string `yaml:"storage_dir"`
	DatabasePath string `yaml:"database_path"`
}

type EmbeddingConfig struct {
	Provider   string `yaml:"provider"`
	Model      string `yaml:"model"`
	ModelDir   string `yaml:"model_dir"`
	Quantize   bool   `yaml:"quantize"`
	Dimensions int    `yaml:"dimensions"`
}

type SearchConfig struct {
	VectorWeight              float64 `yaml:"vector_weight"`
	BM25Weight                float64 `yaml:"bm25_weight"`
	MaxResults                int     `yaml:"max_results"`
	MMREnabled                bool    `yaml:"mmr_enabled"`
	MMRLambda                 float64 `yaml:"mmr_lambda"`
	TemporalDecayEnabled      bool    `yaml:"temporal_decay_enabled"`
	TemporalDecayHalfLifeDays int     `yaml:"temporal_decay_half_life_days"`
}

type SanitizerConfig struct {
	Enabled  bool     `yaml:"enabled"`
	Patterns []string `yaml:"patterns"`
}

type StackConfig struct {
	BudgetBytes  int    `yaml:"budget_bytes"`
	L1CacheTTL   string `yaml:"l1_cache_ttl"`
	L2MaxResults int    `yaml:"l2_max_results"`
}

func Defaults() *Config {
	return &Config{
		Memory: MemoryConfig{
			StorageDir:   "~/.anchored/data",
			DatabasePath: "~/.anchored/data/anchored.db",
		},
		Embedding: EmbeddingConfig{
			Provider:   "onnx",
			Model:      "paraphrase-multilingual-MiniLM-L12-v2",
			ModelDir:   "~/.anchored/data/onnx",
			Quantize:   true,
			Dimensions: 384,
		},
		Search: SearchConfig{
			VectorWeight: 0.7,
			BM25Weight:   0.3,
			MaxResults:   20,
		},
		Sanitizer: SanitizerConfig{
			Enabled: false,
		},
		Stack: StackConfig{
			BudgetBytes:  3600,
			L1CacheTTL:   "6h",
			L2MaxResults: 5,
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := Defaults()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("config file not found, using defaults", "path", path)
			return expandPaths(cfg), nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	return expandPaths(cfg), nil
}

func expandPaths(cfg *Config) *Config {
	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}

	cfg.Memory.StorageDir = expandHome(cfg.Memory.StorageDir, home)
	cfg.Memory.DatabasePath = expandHome(cfg.Memory.DatabasePath, home)
	cfg.Embedding.ModelDir = expandHome(cfg.Embedding.ModelDir, home)

	return cfg
}

func expandHome(path, home string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		return filepath.Join(home, path[2:])
	}
	return path
}

func EnsureDirs(cfg *Config) error {
	dirs := []string{
		cfg.Memory.StorageDir,
		cfg.Embedding.ModelDir,
	}

	// Also ensure the parent dir of the database exists.
	if cfg.Memory.DatabasePath != "" {
		dirs = append(dirs, filepath.Dir(cfg.Memory.DatabasePath))
	}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	return nil
}
