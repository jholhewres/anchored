package dream

type DreamConfig struct {
	Aggressiveness      string  `json:"aggressiveness" yaml:"aggressiveness"`
	DedupThreshold      float64 `json:"dedup_threshold" yaml:"dedup_threshold"`
	MaxDeletionsPerRun  int     `json:"max_deletions_per_run" yaml:"max_deletions_per_run"`
	ContradictionAction string  `json:"contradiction_action" yaml:"contradiction_action"`
	MaxPairwiseCompare  int     `json:"max_pairwise_compare" yaml:"max_pairwise_compare"`
}

func DefaultDreamConfig() DreamConfig {
	return DreamConfigForAggressiveness("moderate")
}

func DreamConfigForAggressiveness(level string) DreamConfig {
	switch level {
	case "conservative":
		return DreamConfig{
			Aggressiveness:      "conservative",
			DedupThreshold:      0.85,
			MaxDeletionsPerRun:  0,
			ContradictionAction: "flag",
			MaxPairwiseCompare:  10000,
		}
	case "aggressive":
		return DreamConfig{
			Aggressiveness:      "aggressive",
			DedupThreshold:      0.65,
			MaxDeletionsPerRun:  200,
			ContradictionAction: "flag",
			MaxPairwiseCompare:  10000,
		}
	default:
		return DreamConfig{
			Aggressiveness:      "moderate",
			DedupThreshold:      0.75,
			MaxDeletionsPerRun:  50,
			ContradictionAction: "flag",
			MaxPairwiseCompare:  10000,
		}
	}
}
