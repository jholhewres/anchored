package memory

import "testing"

// TestWithSaveScope_UserNarrowsAnyCategory locks what an explicit scope
// does on a save: "user" keeps any memory in the personal scope, which no
// remote receives, even one that would otherwise take the project's scope;
// any other value on a non-preference leaves the scope to the project.
func TestWithSaveScope_UserNarrowsAnyCategory(t *testing.T) {
	if got := ParseMetadata(WithSaveScope(nil, "fact", "user")).Scope; got != ScopeUser {
		t.Fatalf("fact saved with scope user: scope %q, want user", got)
	}
	if got := ParseMetadata(WithSaveScope(nil, "decision", " User ")).Scope; got != ScopeUser {
		t.Fatalf("decision saved with scope ' User ': scope %q, want user", got)
	}
	if got := ParseMetadata(WithSaveScope(nil, "fact", "project")).Scope; got != "" {
		t.Fatalf("fact saved with scope project: scope %q, want it left to inference", got)
	}
	existing := MemoryMetadata{Scope: ScopeProject}.ToAny()
	if got := ParseMetadata(WithSaveScope(existing, "fact", "user")).Scope; got != ScopeUser {
		t.Fatalf("project fact saved again with scope user: scope %q, want user", got)
	}
	if got := ParseMetadata(WithSaveScope(existing, "fact", "")).Scope; got != ScopeProject {
		t.Fatalf("project fact saved again without a scope: scope %q, want project kept", got)
	}
	if got := ParseMetadata(WithSaveScope(nil, "preference", "team")).PreferenceScope; got != PreferenceScopeTeam {
		t.Fatalf("team preference: preference scope %q, want team", got)
	}
}

// TestWithSaveScope_UnknownScopeFailsClosed locks the save scope against a
// value outside the enum: anything but project or team keeps the memory
// personal, including look-alikes of "user".
func TestWithSaveScope_UnknownScopeFailsClosed(t *testing.T) {
	for _, scope := range []string{"private", "personal", "local", "user\u200b", "ｕｓｅｒ"} {
		if got := ParseMetadata(WithSaveScope(nil, "decision", scope)).Scope; got != ScopeUser {
			t.Errorf("decision saved with scope %q: scope %q, want user", scope, got)
		}
	}
	for _, scope := range []string{"project", " Team "} {
		if got := ParseMetadata(WithSaveScope(nil, "decision", scope)).Scope; got != "" {
			t.Errorf("decision saved with scope %q: scope %q, want it left to inference", scope, got)
		}
	}
}

// TestInferDefaultsFromCategory_UnknownCategoryGetsAScope locks the default
// branch: a category outside the known ones is personal outside a project,
// so the remote classifier never meets a memory with no scope.
func TestInferDefaultsFromCategory_UnknownCategoryGetsAScope(t *testing.T) {
	if got := InferDefaultsFromCategory("session_note", false).Scope; got != ScopeUser {
		t.Fatalf("unknown category outside a project: scope %q, want user", got)
	}
	if got := InferDefaultsFromCategory("session_note", true).Scope; got != ScopeProject {
		t.Fatalf("unknown category in a project: scope %q, want project", got)
	}
}
