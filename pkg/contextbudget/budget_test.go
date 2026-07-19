package contextbudget

import (
	"strings"
	"testing"
)

func TestApproxTokens(t *testing.T) {
	cases := map[string]int{
		"":      0,
		"a":     1,     // (1+3)/4
		"abcd":  1,     // (4+3)/4
		"abcde": 2,     // (5+3)/4
		"\n":    1,
	}
	for in, want := range cases {
		if got := ApproxTokens(in); got != want {
			t.Errorf("ApproxTokens(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAssemble(t *testing.T) {
	// Budgets are in approximate tokens (ApproxTokens: ~4 chars/token, rounded
	// up; the "\n" separator costs 1). Token costs used below:
	//   "hello"/"world"/"12345"/"aaaaa"/"bbbbb"/"small"/"123456" → 2
	//   "abc"/"def"/"low"/"high"/"mid"/1-char items/"AAAA"/"BBBB" → 1
	//   strings.Repeat("X",20) → 5   "BSMALL" → 2
	tests := []struct {
		name        string
		tiers       []Tier
		budget      int
		wantOut     string
		wantDropped int
	}{
		{
			name:        "budget zero returns empty and all non-empty items as dropped",
			tiers:       []Tier{{Name: "a", Items: []Item{{Text: "hello"}, {Text: "world"}}}},
			budget:      0,
			wantOut:     "",
			wantDropped: 2,
		},
		{
			name:        "budget negative returns empty and all non-empty items as dropped",
			tiers:       []Tier{{Name: "a", Items: []Item{{Text: "abc"}, {Text: "def"}}}},
			budget:      -1,
			wantOut:     "",
			wantDropped: 2,
		},
		{
			name: "empty text items ignored not counted as dropped",
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: ""},
				{Text: "hello"},
				{Text: ""},
			}}},
			budget:      100,
			wantOut:     "hello",
			wantDropped: 0,
		},
		{
			name: "single item fits exactly in tokens",
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "12345"}, // 2 tokens
			}}},
			budget:      2,
			wantOut:     "12345",
			wantDropped: 0,
		},
		{
			name: "item larger than budget is dropped whole",
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "123456"}, // 2 tokens
			}}},
			budget:      1,
			wantOut:     "",
			wantDropped: 1,
		},
		{
			name: "fits in bytes but overflows in tokens is dropped",
			// "aaaaa" is 5 bytes but costs 2 tokens; a budget of 1 (which the
			// old byte-based rule would have failed to catch as under-5) drops it.
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "aaaaa"},
			}}},
			budget:      1,
			wantOut:     "",
			wantDropped: 1,
		},
		{
			name: "ceiling respected token by token with separator",
			// item0: 2 tokens; item1: 1 sep + 2 = 3; total needed = 5.
			// budget 4: second item must be dropped.
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "aaaaa", Priority: 1},
				{Text: "bbbbb", Priority: 2},
			}}},
			budget:      4,
			wantOut:     "aaaaa",
			wantDropped: 1,
		},
		{
			name: "ceiling respected token by token fits exactly",
			// 2 + 1 + 2 = 5 tokens exactly
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "aaaaa", Priority: 1},
				{Text: "bbbbb", Priority: 2},
			}}},
			budget:      5,
			wantOut:     "aaaaa\nbbbbb",
			wantDropped: 0,
		},
		{
			name: "priority order within tier",
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: "low", Priority: 10},
				{Text: "high", Priority: 1},
				{Text: "mid", Priority: 5},
			}}},
			budget:      100,
			wantOut:     "high\nmid\nlow",
			wantDropped: 0,
		},
		{
			name: "tier order preserved in output",
			tiers: []Tier{
				{Name: "first", Items: []Item{{Text: "A", Priority: 1}}},
				{Name: "second", Items: []Item{{Text: "B", Priority: 1}}},
				{Name: "third", Items: []Item{{Text: "C", Priority: 1}}},
			},
			budget:      100,
			wantOut:     "A\nB\nC",
			wantDropped: 0,
		},
		{
			name: "higher tier preserved when lower tier would exhaust budget",
			// Tier 0: 1 token; Tier 1: 1 sep + 1 = 2 → total 3 fits both.
			// budget = 2 fits only tier 0.
			tiers: []Tier{
				{Name: "top", Items: []Item{{Text: "AAAA", Priority: 1}}},
				{Name: "bottom", Items: []Item{{Text: "BBBB", Priority: 1}}},
			},
			budget:      2,
			wantOut:     "AAAA",
			wantDropped: 1,
		},
		{
			// MinItems guarantee: tier A has 10 large items (5 tokens each),
			// tier B MinItems=1. Budget can't hold all of A, but B's first item
			// must be reserved before A exhausts the budget.
			name: "MinItems guarantees top item of lower tier even with large upper tier",
			tiers: func() []Tier {
				var aItems []Item
				for i := 0; i < 10; i++ {
					aItems = append(aItems, Item{Text: strings.Repeat("X", 20), Priority: i})
				}
				bItems := []Item{
					{Text: "BSMALL", Priority: 0}, // 2 tokens — must survive
					{Text: strings.Repeat("Y", 20), Priority: 1},
				}
				return []Tier{
					{Name: "A", Items: aItems, MinItems: 0},
					{Name: "B", Items: bItems, MinItems: 1},
				}
			}(),
			// budget 8: pass1 B reserves BSMALL (2). remaining 6.
			// pass2 A item0: 1 sep + 5 = 6 → fits, remaining 0. Everything else drops.
			// Output in tier order: A item0 then BSMALL.
			budget:      8,
			wantOut:     strings.Repeat("X", 20) + "\nBSMALL",
			wantDropped: 10, // 9 remaining A items + 1 remaining B item
		},
		{
			name: "deterministic same input same output",
			tiers: []Tier{
				{Name: "a", Items: []Item{
					{Text: "z", Priority: 3},
					{Text: "a", Priority: 1},
					{Text: "m", Priority: 2},
				}},
			},
			budget:      100,
			wantOut:     "a\nm\nz",
			wantDropped: 0,
		},
		{
			name:        "no tiers returns empty",
			tiers:       nil,
			budget:      100,
			wantOut:     "",
			wantDropped: 0,
		},
		{
			name: "all items empty",
			tiers: []Tier{{Name: "a", Items: []Item{
				{Text: ""},
				{Text: ""},
			}}},
			budget:      100,
			wantOut:     "",
			wantDropped: 0,
		},
		{
			name: "MinItems zero still fills on pass 2",
			tiers: []Tier{
				{Name: "a", Items: []Item{{Text: "hello", Priority: 1}}, MinItems: 0},
			},
			budget:      100,
			wantOut:     "hello",
			wantDropped: 0,
		},
		{
			// Pass 1 reserves tier B's item even though tier A's item is too big.
			// A item = 5 tokens (won't fit budget 4); B item = 2 tokens (reserved).
			name: "MinItems reserves lower tier item when upper tier items too large",
			tiers: []Tier{
				{Name: "A", Items: []Item{{Text: strings.Repeat("X", 20), Priority: 1}}, MinItems: 0},
				{Name: "B", Items: []Item{{Text: "small", Priority: 1}}, MinItems: 1},
			},
			budget:      4,
			wantOut:     "small",
			wantDropped: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotDropped := Assemble(tc.tiers, tc.budget)
			if got != tc.wantOut {
				t.Errorf("out = %q, want %q", got, tc.wantOut)
			}
			if gotDropped != tc.wantDropped {
				t.Errorf("dropped = %d, want %d", gotDropped, tc.wantDropped)
			}
			// Verify the token ceiling is respected.
			if tc.budget > 0 && ApproxTokens(got) > tc.budget {
				t.Errorf("output token cost %d exceeds budget %d", ApproxTokens(got), tc.budget)
			}
		})
	}
}

// TestAssembleDeterminism runs Assemble twice with the same input and verifies
// the output is identical.
func TestAssembleDeterminism(t *testing.T) {
	tiers := []Tier{
		{Name: "first", Items: []Item{
			{Text: "c", Priority: 3},
			{Text: "a", Priority: 1},
			{Text: "b", Priority: 2},
		}, MinItems: 2},
		{Name: "second", Items: []Item{
			{Text: "z", Priority: 10},
			{Text: "y", Priority: 5},
		}, MinItems: 1},
	}
	out1, d1 := Assemble(tiers, 50)
	out2, d2 := Assemble(tiers, 50)
	if out1 != out2 || d1 != d2 {
		t.Errorf("non-deterministic: run1=(%q,%d), run2=(%q,%d)", out1, d1, out2, d2)
	}
}
