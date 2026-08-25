package main

import (
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

// Capture writes chunks with a 72h TTL, but the Evictor that enforces that TTL
// is built only inside the context optimizer. If capture is not gated on the
// same flag, disabling the optimizer disables the cleanup while leaving the
// writes on — the exact asymmetry that let content_chunks reach ~300 MB of
// expired rows on a real database.
func TestArtifactCaptureEnabledTracksTheEvictionFlag(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{
			name: "optimizer on — capture and eviction both run",
			cfg:  cfgWithOptimizer(true),
			want: true,
		},
		{
			name: "optimizer off — capture must stop, since nothing evicts",
			cfg:  cfgWithOptimizer(false),
			want: false,
		},
		{
			name: "nil config — no basis to assume eviction runs",
			cfg:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := artifactCaptureEnabled(tt.cfg); got != tt.want {
				t.Errorf("artifactCaptureEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func cfgWithOptimizer(enabled bool) *config.Config {
	c := &config.Config{}
	c.ContextOptimizer.Enabled = enabled
	return c
}
