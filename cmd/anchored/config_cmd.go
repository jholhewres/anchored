package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	case "wizard", "interactive":
		runConfigWizard()
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
	configFile, cfg := loadWritableConfig()

	setConfigValue(cfg, key, value)
	writeConfigFile(configFile, cfg)

	fmt.Printf("Set %s = %s\n", key, value)
}

func runConfigWizard() {
	configFile, cfg := loadWritableConfig()
	reader := bufio.NewReader(os.Stdin)

	fmt.Fprintf(os.Stderr, "Anchored configuration wizard\n")
	fmt.Fprintf(os.Stderr, "Config file: %s\n", configFile)
	fmt.Fprintf(os.Stderr, "Press Enter to keep the current value.\n\n")

	cfg.Memory.StorageDir = promptString(reader, "Memory storage dir", cfg.Memory.StorageDir)
	cfg.Memory.DatabasePath = promptString(reader, "SQLite database path", cfg.Memory.DatabasePath)

	fmt.Fprintln(os.Stderr)
	cfg.Embedding.Provider = promptString(reader, "Embedding provider", cfg.Embedding.Provider)
	cfg.Embedding.Model = promptString(reader, "Embedding model", cfg.Embedding.Model)
	cfg.Embedding.ModelDir = promptString(reader, "Embedding model dir", cfg.Embedding.ModelDir)
	cfg.Embedding.Quantize = promptBool(reader, "Quantize embeddings", cfg.Embedding.Quantize)
	cfg.Embedding.Dimensions = promptInt(reader, "Embedding dimensions", cfg.Embedding.Dimensions)

	fmt.Fprintln(os.Stderr)
	cfg.Search.VectorWeight = promptFloat(reader, "Search vector weight", cfg.Search.VectorWeight)
	cfg.Search.BM25Weight = promptFloat(reader, "Search BM25 weight", cfg.Search.BM25Weight)
	cfg.Search.MaxResults = promptInt(reader, "Search max results", cfg.Search.MaxResults)
	cfg.Search.MMREnabled = promptBool(reader, "Enable MMR diversification", cfg.Search.MMREnabled)
	cfg.Search.MMRLambda = promptFloat(reader, "MMR lambda", cfg.Search.MMRLambda)
	cfg.Search.TemporalDecayEnabled = promptBool(reader, "Enable temporal decay", cfg.Search.TemporalDecayEnabled)
	cfg.Search.TemporalDecayHalfLifeDays = promptInt(reader, "Temporal decay half-life days", cfg.Search.TemporalDecayHalfLifeDays)

	fmt.Fprintln(os.Stderr)
	cfg.Sanitizer.Enabled = promptBool(reader, "Enable sanitizer", cfg.Sanitizer.Enabled)

	fmt.Fprintln(os.Stderr)
	cfg.ContextOptimizer.Enabled = promptBool(reader, "Enable context optimizer", cfg.ContextOptimizer.Enabled)
	cfg.ContextOptimizer.DefaultTTL = promptInt(reader, "Context default TTL hours", cfg.ContextOptimizer.DefaultTTL)
	cfg.ContextOptimizer.LRUCapMB = promptInt(reader, "Context LRU cap MB", cfg.ContextOptimizer.LRUCapMB)
	cfg.ContextOptimizer.SandboxTimeout = promptInt(reader, "Sandbox timeout seconds", cfg.ContextOptimizer.SandboxTimeout)
	cfg.ContextOptimizer.MaxOutputKB = promptInt(reader, "Sandbox max output KB", cfg.ContextOptimizer.MaxOutputKB)
	cfg.ContextOptimizer.FetchCacheTTL = promptInt(reader, "Fetch cache TTL hours", cfg.ContextOptimizer.FetchCacheTTL)

	fmt.Fprintln(os.Stderr)
	cfg.Curation.Enabled = promptBool(reader, "Enable curation worker", cfg.Curation.Enabled)
	cfg.Curation.IntervalHours = promptInt(reader, "Curation interval hours", cfg.Curation.IntervalHours)
	cfg.Curation.IntervalMinutes = promptInt(reader, "Curation interval minutes override (0 = use hours)", cfg.Curation.IntervalMinutes)
	cfg.Curation.Threshold = promptFloat(reader, "Curation low-signal threshold", cfg.Curation.Threshold)
	cfg.Curation.MaxUpdates = promptInt(reader, "Curation max updates per run", cfg.Curation.MaxUpdates)

	fmt.Fprintln(os.Stderr)
	cfg.Dream.Aggressiveness = promptString(reader, "Dream aggressiveness", cfg.Dream.Aggressiveness)
	cfg.Dream.DedupThreshold = promptFloat(reader, "Dream dedup threshold", cfg.Dream.DedupThreshold)
	cfg.Dream.MaxDeletionsPerRun = promptInt(reader, "Dream max deletions per run", cfg.Dream.MaxDeletionsPerRun)
	cfg.Dream.ContradictionAction = promptString(reader, "Dream contradiction action", cfg.Dream.ContradictionAction)

	fmt.Fprintln(os.Stderr)
	cfg.Debug.Enabled = promptBool(reader, "Enable debug log", cfg.Debug.Enabled)
	cfg.Debug.Path = promptString(reader, "Debug log path", cfg.Debug.Path)
	cfg.Plugin.AutoUpdate = promptBool(reader, "Auto-update Claude Code plugin", cfg.Plugin.AutoUpdate)
	cfg.Plugin.MarketplaceDir = promptString(reader, "Claude plugin marketplace dir", cfg.Plugin.MarketplaceDir)
	cfg.Plugin.CacheDir = promptString(reader, "Claude plugin cache dir", cfg.Plugin.CacheDir)

	if !promptBool(reader, "Write config", true) {
		fmt.Fprintln(os.Stderr, "Config unchanged.")
		return
	}

	writeConfigFile(configFile, cfg)
	fmt.Fprintf(os.Stderr, "Wrote %s\n", configFile)
}

func loadWritableConfig() (string, *config.Config) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine home dir: %v\n", err)
		os.Exit(1)
	}

	configFile := home + "/.anchored/config.yaml"
	cfg := config.Defaults()

	data, err := os.ReadFile(configFile)
	if err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "error parsing config: %v\n", err)
			os.Exit(1)
		}
	}

	return configFile, cfg
}

func writeConfigFile(configFile string, cfg *config.Config) {

	out, err := yaml.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling config: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error creating config dir: %v\n", err)
		os.Exit(1)
	}
	if err := writePrivateFile(configFile, out); err != nil {
		fmt.Fprintf(os.Stderr, "error writing config: %v\n", err)
		os.Exit(1)
	}
}

func promptString(reader *bufio.Reader, label, current string) string {
	fmt.Fprintf(os.Stderr, "%s [%s]: ", label, current)
	line, err := reader.ReadString('\n')
	if err != nil {
		return current
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current
	}
	return line
}

func promptBool(reader *bufio.Reader, label string, current bool) bool {
	defaultValue := "false"
	if current {
		defaultValue = "true"
	}
	for {
		value := strings.ToLower(promptString(reader, label, defaultValue))
		switch value {
		case "true", "t", "yes", "y", "1", "sim", "s":
			return true
		case "false", "f", "no", "n", "0", "não", "nao":
			return false
		default:
			fmt.Fprintln(os.Stderr, "Please enter true/false or y/n.")
		}
	}
}

func promptInt(reader *bufio.Reader, label string, current int) int {
	for {
		value := promptString(reader, label, fmt.Sprintf("%d", current))
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
		fmt.Fprintln(os.Stderr, "Please enter a valid integer.")
	}
}

func promptFloat(reader *bufio.Reader, label string, current float64) float64 {
	for {
		value := promptString(reader, label, fmt.Sprintf("%.2f", current))
		parsed, err := strconv.ParseFloat(value, 64)
		if err == nil {
			return parsed
		}
		fmt.Fprintln(os.Stderr, "Please enter a valid number.")
	}
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
	case "curation.enabled":
		cfg.Curation.Enabled = value == "true"
	case "curation.interval_hours":
		fmt.Sscanf(value, "%d", &cfg.Curation.IntervalHours)
	case "curation.interval_minutes":
		fmt.Sscanf(value, "%d", &cfg.Curation.IntervalMinutes)
	case "curation.threshold":
		fmt.Sscanf(value, "%f", &cfg.Curation.Threshold)
	case "curation.max_updates_per_run":
		fmt.Sscanf(value, "%d", &cfg.Curation.MaxUpdates)
	default:
		fmt.Fprintf(os.Stderr, "Unknown config key: %s\n", key)
		os.Exit(1)
	}
}
