package memory

import "testing"

// TestWithPreferenceScope_UserNarrowsAnyCategory locks what an explicit scope
// does on a save: "user" keeps any memory in the personal scope, which no
// remote receives, even one that would otherwise take the project's scope;
// any other value on a non-preference leaves the scope to the project.
func TestWithPreferenceScope_UserNarrowsAnyCategory(t *testing.T) {
	if got := ParseMetadata(WithPreferenceScope(nil, "fact", "user")).Scope; got != ScopeUser {
		t.Fatalf("fact saved with scope user: scope %q, want user", got)
	}
	if got := ParseMetadata(WithPreferenceScope(nil, "decision", " User ")).Scope; got != ScopeUser {
		t.Fatalf("decision saved with scope ' User ': scope %q, want user", got)
	}
	if got := ParseMetadata(WithPreferenceScope(nil, "fact", "project")).Scope; got != "" {
		t.Fatalf("fact saved with scope project: scope %q, want it left to inference", got)
	}
	existing := MemoryMetadata{Scope: ScopeProject}.ToAny()
	if got := ParseMetadata(WithPreferenceScope(existing, "fact", "user")).Scope; got != ScopeUser {
		t.Fatalf("project fact saved again with scope user: scope %q, want user", got)
	}
	if got := ParseMetadata(WithPreferenceScope(existing, "fact", "")).Scope; got != ScopeProject {
		t.Fatalf("project fact saved again without a scope: scope %q, want project kept", got)
	}
	if got := ParseMetadata(WithPreferenceScope(nil, "preference", "team")).PreferenceScope; got != PreferenceScopeTeam {
		t.Fatalf("team preference: preference scope %q, want team", got)
	}
}
