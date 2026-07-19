package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPluginConfig_SessionStartBudget_Defaults(t *testing.T) {
	var p PluginConfig
	if got := p.SessionStartBudget(); got != 7000 {
		t.Errorf("SessionStartBudget() = %d, want 7000 (nil default)", got)
	}
}

func TestPluginConfig_SessionStartBudgetTokensResolved(t *testing.T) {
	tokens := func(n int) *int { return &n }

	// nil/nil → default 2000
	var def PluginConfig
	if got := def.SessionStartBudgetTokensResolved(); got != 2000 {
		t.Errorf("default = %d, want 2000", got)
	}
	// explicit token value wins (including 0 = opt-out)
	if got := (PluginConfig{SessionStartBudgetTokens: tokens(1500)}).SessionStartBudgetTokensResolved(); got != 1500 {
		t.Errorf("explicit tokens = %d, want 1500", got)
	}
	if got := (PluginConfig{SessionStartBudgetTokens: tokens(0)}).SessionStartBudgetTokensResolved(); got != 0 {
		t.Errorf("explicit token opt-out = %d, want 0", got)
	}
	// legacy bytes convert ÷4 when tokens unset (preserving 0 opt-out)
	if got := (PluginConfig{SessionStartBudgetBytes: tokens(8000)}).SessionStartBudgetTokensResolved(); got != 2000 {
		t.Errorf("legacy bytes 8000 = %d tokens, want 2000", got)
	}
	if got := (PluginConfig{SessionStartBudgetBytes: tokens(0)}).SessionStartBudgetTokensResolved(); got != 0 {
		t.Errorf("legacy bytes opt-out = %d, want 0", got)
	}
}

func TestPluginConfig_AutoSaveStopEnabled_Defaults(t *testing.T) {
	var p PluginConfig
	if !p.AutoSaveStopEnabled() {
		t.Error("AutoSaveStopEnabled() = false, want true (nil default)")
	}
}

func TestPluginConfig_AdaptiveReminderEnabled_Defaults(t *testing.T) {
	var p PluginConfig
	if !p.AdaptiveReminderEnabled() {
		t.Error("AdaptiveReminderEnabled() = false, want true (nil default)")
	}
}

// The context gate is forced ON by default: empty, "on", "enforce", and the
// legacy "off" (the value older defaults serialized into existing configs)
// all resolve to "enforce", so a version bump alone arms the gate with no
// config edit. Only a deliberate "disabled" opts out.
func TestPluginConfig_ContextGateMode_ForcedOnByDefault(t *testing.T) {
	for _, v := range []string{"", "on", "enforce", "off", "OFF", "garbage"} {
		p := PluginConfig{ContextGate: v}
		if got := p.ContextGateMode(); got != "enforce" {
			t.Errorf("ContextGateMode(%q) = %q, want enforce (gate forced on)", v, got)
		}
	}
	if got := (PluginConfig{ContextGate: "disabled"}).ContextGateMode(); got != "off" {
		t.Errorf("ContextGateMode(disabled) = %q, want off (the only opt-out)", got)
	}
}

// Auto-recall defaults to the richest "full" mode for empty/unknown values;
// explicit "off"/"hits" still opt down.
func TestPluginConfig_AutoRecallMode_DefaultsFull(t *testing.T) {
	for _, v := range []string{"", "garbage"} {
		if got := (PluginConfig{AutoRecall: v}).AutoRecallMode(); got != "full" {
			t.Errorf("AutoRecallMode(%q) = %q, want full (default)", v, got)
		}
	}
	for _, v := range []string{"off", "hits", "full"} {
		if got := (PluginConfig{AutoRecall: v}).AutoRecallMode(); got != v {
			t.Errorf("AutoRecallMode(%q) = %q, want %q (explicit honored)", v, got, v)
		}
	}
}

func TestPluginConfig_SessionStartBudget_ExplicitZero(t *testing.T) {
	v := 0
	p := PluginConfig{SessionStartBudgetBytes: &v}
	if got := p.SessionStartBudget(); got != 0 {
		t.Errorf("SessionStartBudget() = %d, want 0 (explicit zero)", got)
	}
}

func TestPluginConfig_AutoSaveStop_ExplicitFalse(t *testing.T) {
	f := false
	p := PluginConfig{AutoSaveStop: &f}
	if p.AutoSaveStopEnabled() {
		t.Error("AutoSaveStopEnabled() = true, want false (explicit false)")
	}
}

func TestPluginConfig_AdaptiveReminder_ExplicitFalse(t *testing.T) {
	f := false
	p := PluginConfig{AdaptiveReminder: &f}
	if p.AdaptiveReminderEnabled() {
		t.Error("AdaptiveReminderEnabled() = true, want false (explicit false)")
	}
}

func TestPluginConfig_YAMLRoundTrip_Absent(t *testing.T) {
	// When keys are absent, defaults apply.
	const src = `plugin: {}`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Plugin
	if p.SessionStartBudgetBytes != nil {
		t.Errorf("SessionStartBudgetBytes should be nil when absent, got %v", p.SessionStartBudgetBytes)
	}
	if p.AutoSaveStop != nil {
		t.Errorf("AutoSaveStop should be nil when absent, got %v", p.AutoSaveStop)
	}
	if p.AdaptiveReminder != nil {
		t.Errorf("AdaptiveReminder should be nil when absent, got %v", p.AdaptiveReminder)
	}
	if got := p.SessionStartBudget(); got != 7000 {
		t.Errorf("SessionStartBudget() = %d, want 7000", got)
	}
	if !p.AutoSaveStopEnabled() {
		t.Error("AutoSaveStopEnabled() = false, want true")
	}
	if !p.AdaptiveReminderEnabled() {
		t.Error("AdaptiveReminderEnabled() = false, want true")
	}
}

func TestPluginConfig_YAMLRoundTrip_ExplicitZeroAndFalse(t *testing.T) {
	const src = `
plugin:
  sessionstart_budget_bytes: 0
  auto_save_stop: false
  adaptive_reminder: false
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Plugin
	if p.SessionStartBudgetBytes == nil {
		t.Fatal("SessionStartBudgetBytes should be non-nil when set to 0")
	}
	if got := p.SessionStartBudget(); got != 0 {
		t.Errorf("SessionStartBudget() = %d, want 0", got)
	}
	if p.AutoSaveStop == nil {
		t.Fatal("AutoSaveStop should be non-nil when set to false")
	}
	if p.AutoSaveStopEnabled() {
		t.Error("AutoSaveStopEnabled() = true, want false")
	}
	if p.AdaptiveReminder == nil {
		t.Fatal("AdaptiveReminder should be non-nil when set to false")
	}
	if p.AdaptiveReminderEnabled() {
		t.Error("AdaptiveReminderEnabled() = true, want false")
	}
}

func TestPluginConfig_YAMLRoundTrip_ExplicitValues(t *testing.T) {
	const src = `
plugin:
  sessionstart_budget_bytes: 5000
  auto_save_stop: true
  adaptive_reminder: true
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p := cfg.Plugin
	if got := p.SessionStartBudget(); got != 5000 {
		t.Errorf("SessionStartBudget() = %d, want 5000", got)
	}
	if !p.AutoSaveStopEnabled() {
		t.Error("AutoSaveStopEnabled() = false, want true")
	}
	if !p.AdaptiveReminderEnabled() {
		t.Error("AdaptiveReminderEnabled() = false, want true")
	}
}

func TestPluginConfig_HookBudget_Unchanged(t *testing.T) {
	// Verify HookBudget() is still correct after the struct change.
	var p PluginConfig
	if got := p.HookBudget(); got != 4800 {
		t.Errorf("HookBudget() = %d, want 4800 (default)", got)
	}
	p.HookBudgetBytes = 9600
	if got := p.HookBudget(); got != 9600 {
		t.Errorf("HookBudget() = %d, want 9600", got)
	}
}
