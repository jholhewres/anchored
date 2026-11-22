package sync

import (
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
)

func TestClassifyForRemote(t *testing.T) {
	root := "/tmp/project"

	t.Run("plain fact is syncable", func(t *testing.T) {
		_, _, ok := ClassifyForRemote(memory.Memory{
			ID: "1", Category: "fact",
			Content: "we run Go 1.22 on the ARM build server",
		}, root)
		if !ok {
			t.Fatal("a plain project fact should be syncable")
		}
	})

	t.Run("user-scoped never leaves the machine", func(t *testing.T) {
		_, _, ok := ClassifyForRemote(memory.Memory{
			ID: "2", Category: "fact", Content: "my personal todo",
			Metadata: map[string]any{"scope": "user"},
		}, root)
		if ok {
			t.Fatal("user-scoped memory must not be syncable")
		}
	})

	t.Run("blocked category never leaves the machine", func(t *testing.T) {
		_, _, ok := ClassifyForRemote(memory.Memory{
			ID: "3", Category: "preference", Content: "I prefer tabs",
		}, root)
		if ok {
			t.Fatal("preference category must not be syncable")
		}
	})

	t.Run("secret never leaves the machine", func(t *testing.T) {
		_, _, ok := ClassifyForRemote(memory.Memory{
			ID: "4", Category: "fact",
			Content: "the deploy key is AKIAIOSFODNN7EXAMPLE do not share",
		}, root)
		if ok {
			t.Fatal("memory containing a secret must not be syncable")
		}
	})

	t.Run("syncable content has no raw home path", func(t *testing.T) {
		content, _, ok := ClassifyForRemote(memory.Memory{
			ID: "5", Category: "fact",
			Content: "the config lives under the project root and loads on boot",
		}, root)
		if ok && strings.Contains(content, "/home/") {
			t.Fatal("returned content must not contain a raw home path")
		}
	})
}

// TestClassifyForRemote_RedactsAndSaysWhy locks what the remote classifier
// hands back: a project path made relative before it is sent, and for a
// memory it keeps local, the reason.
func TestClassifyForRemote_RedactsAndSaysWhy(t *testing.T) {
	content, _, ok := ClassifyForRemote(memory.Memory{
		Category: "decision",
		Content:  "use /home/test/work/repo/config.yaml for the local fixture",
	}, "/home/test/work/repo")
	if !ok || strings.Contains(content, "/home/test/work/repo") || !strings.Contains(content, "./config.yaml") {
		t.Fatalf("redacted content = %q ok=%v, want ./config.yaml", content, ok)
	}
	for _, tc := range []struct {
		m      memory.Memory
		reason string
	}{
		{memory.Memory{Category: "fact", Content: "the staging cluster restarts nightly", Metadata: memory.MemoryMetadata{Scope: memory.ScopeUser}.ToAny()}, "user_scope_blocked"},
		{memory.Memory{Category: "decision", Content: "the deploy bot uses ghp_" + strings.Repeat("a", 36)}, "secret_pattern"},
		{memory.Memory{Category: "Event", Content: "deployed the api to staging today"}, "category_remote_blocked"},
	} {
		if _, reason, ok := ClassifyForRemote(tc.m, ""); ok || reason != tc.reason {
			t.Errorf("ClassifyForRemote(%q) = ok %v reason %q, want blocked as %s", tc.m.Content, ok, reason, tc.reason)
		}
	}
}

// TestClassifyForNamedRemote_SkipsOnlyTheQualityGates locks the rule for a
// save that names its remote: a memory held back only for its quality goes,
// and one held back for privacy stays.
func TestClassifyForNamedRemote_SkipsOnlyTheQualityGates(t *testing.T) {
	lowSignal := memory.Memory{Category: "decision", Content: "Settled on Postgres",
		Metadata: memory.MemoryMetadata{CurationStatus: memory.CurationStatusLowSignal}.ToAny()}
	if _, reason, ok := ClassifyForRemote(lowSignal, ""); ok || reason != "low_signal" {
		t.Fatalf("auto-sync of a low-signal memory: ok %v reason %q, want low_signal", ok, reason)
	}
	if _, reason, ok := ClassifyForNamedRemote(lowSignal, ""); !ok {
		t.Fatalf("a named remote held a low-signal memory back: %s", reason)
	}
	personal := memory.Memory{Category: "decision", Content: "Settled on Postgres",
		Metadata: memory.MemoryMetadata{Scope: memory.ScopeUser, CurationStatus: memory.CurationStatusLowSignal}.ToAny()}
	if _, reason, ok := ClassifyForNamedRemote(personal, ""); ok || reason != "user_scope_blocked" {
		t.Fatalf("a named remote: ok %v reason %q, want user_scope_blocked", ok, reason)
	}
}
