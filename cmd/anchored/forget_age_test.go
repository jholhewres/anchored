package main

import (
	"testing"
	"time"
)

// parseAge feeds a cutoff into a destructive command, so every input that does
// NOT error has to produce a cutoff in the past. The overflow case is the one
// that matters: time.Duration is int64 nanoseconds, so a large enough day count
// wraps negative and `now.Add(-age)` lands in the FUTURE, where `created_at <
// cutoff` matches the entire corpus.
func TestParseAgeRejectsAnythingThatWouldMatchEverything(t *testing.T) {
	rejected := []struct {
		in  string
		why string
	}{
		{"106752d", "overflows int64 nanoseconds and wraps to a future cutoff"},
		{"9999999999d", "epoch seconds pasted where days were expected"},
		{"0d", "cutoff at now matches every existing row"},
		{"0h", "same, via the duration path"},
		{"-60d", "negative age moves the cutoff forward"},
		{"-12h", "negative duration moves the cutoff forward"},
		{"", "empty"},
		{"d", "no number"},
		{"60", "no unit"},
		{"sixty days", "not a duration"},
		{"1e3d", "Atoi does not accept exponent notation"},
	}
	for _, tt := range rejected {
		t.Run(tt.in, func(t *testing.T) {
			if _, err := parseAge(tt.in); err == nil {
				t.Errorf("parseAge(%q) accepted it — %s", tt.in, tt.why)
			}
		})
	}
}

func TestParseAgeAcceptsRealRetentionCuts(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"1d", 24 * time.Hour},
		{"60d", 60 * 24 * time.Hour},
		{"36500d", 36500 * 24 * time.Hour},
		{"12h", 12 * time.Hour},
		{"90m", 90 * time.Minute},
		{"  60d  ", 60 * 24 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseAge(tt.in)
			if err != nil {
				t.Fatalf("parseAge(%q) errored: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseAge(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// The property that actually protects the corpus: whatever parseAge accepts,
// subtracting it from now must land strictly in the past.
func TestParseAgeAcceptedValuesAlwaysYieldAPastCutoff(t *testing.T) {
	now := time.Now().UTC()
	for _, in := range []string{"1d", "60d", "36500d", "12h", "1s", "1ns"} {
		age, err := parseAge(in)
		if err != nil {
			continue
		}
		if cutoff := now.Add(-age); !cutoff.Before(now) {
			t.Errorf("parseAge(%q) = %v produced cutoff %v, which is not before %v",
				in, age, cutoff, now)
		}
	}
}
