package main

import (
	"fmt"
	"os"

	"github.com/jholhewres/anchored/pkg/config"
	"gopkg.in/yaml.v3"
)

func runConfig(args []string) {
	if len(args) == 0 {
		runConfigShow(nil)
		return
	}

	switch args[0] {
	case "show":
		fs := newFlagSet("config show")
		configPath := fs.String("config", "", "path to config file")
		fs.Parse(args[1:])
		runConfigShow(configPath)
	case "set":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "Usage: anchored config set <key> <value>")
			os.Exit(1)
		}
		runConfigSet(args[1], args[2])
	default:
		fmt.Fprintf(os.Stderr, "Unknown config subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func runConfigShow(configPath *string) {
	path := ""
	if configPath != nil {
		path = *configPath
	}
	cfg, err := loadConfig(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
		os.Exit(1)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling config: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(string(data))
}

func runConfigSet(key, value string) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	configFile := home + "/.anchored/config.yaml"
	cfg := config.Defaults()

	data, err := os.ReadFile(configFile)
	if err == nil {
		yaml.Unmarshal(data, cfg)
	}

	setConfigValue(cfg, key, value)

	out, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling config: %v\n", err)
		os.Exit(1)
	}

	os.MkdirAll(home+"/.anchored", 0o755)
	if err := os.WriteFile(configFile, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing config: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Set %s = %s\n", key, value)
}

func setConfigValue(cfg *config.Config, key, value string) {
	switch key {
	case "memory.storage_dir":
		cfg.Memory.StorageDir = value
	case "memory.database_path":
		cfg.Memory.DatabasePath = value
	case "embedding.provider":
		cfg.Embedding.Provider = value
	case "embedding.model":
		cfg.Embedding.Model = value
	case "embedding.model_dir":
		cfg.Embedding.ModelDir = value
	case "search.vector_weight":
		fmt.Sscanf(value, "%f", &cfg.Search.VectorWeight)
	case "search.bm25_weight":
		fmt.Sscanf(value, "%f", &cfg.Search.BM25Weight)
	case "search.max_results":
		fmt.Sscanf(value, "%d", &cfg.Search.MaxResults)
	case "sanitizer.enabled":
		cfg.Sanitizer.Enabled = value == "true"
	case "stack.budget_bytes":
		fmt.Sscanf(value, "%d", &cfg.Stack.BudgetBytes)
	default:
		fmt.Fprintf(os.Stderr, "Unknown config key: %s\n", key)
		os.Exit(1)
	}
}
