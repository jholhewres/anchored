package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type cursorMockStore struct {
	saved []cursorMockSave
}

type cursorMockSave struct {
	content, category, source, cwd string
}

func (m *cursorMockStore) SaveRaw(_ context.Context, content, category, source, cwd string) error {
	m.saved = append(m.saved, cursorMockSave{content, category, source, cwd})
	return nil
}

func (m *cursorMockStore) SaveRawWithSource(_ context.Context, content, category, source string, _ *string, cwd string) error {
	m.saved = append(m.saved, cursorMockSave{content, category, source, cwd})
	return nil
}

func withTestDir(t *testing.T, fn func(baseDir string)) {
	t.Helper()
	dir := t.TempDir()
	fn(dir)
}

func writeMDC(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantFM     map[string]interface{}
		wantBody   string
		wantErr    bool
	}{
		{
			name:     "no frontmatter",
			input:    "# Just markdown\nsome content",
			wantFM:   nil,
			wantBody: "# Just markdown\nsome content",
		},
		{
			name: "description and globs string",
			input: "---\ndescription: Architecture rules\nglobs: src/**\n---\n# Rules\nBody here",
			wantFM: map[string]interface{}{
				"description": "Architecture rules",
				"globs":       "src/**",
			},
			wantBody: "# Rules\nBody here",
		},
		{
			name: "globs as JSON array",
			input: `---
description: Multi glob rules
globs: ["src/**", "lib/**"]
---
# Multi rules
Body content`,
			wantFM: map[string]interface{}{
				"description": "Multi glob rules",
				"globs":       []string{"src/**", "lib/**"},
			},
			wantBody: "# Multi rules\nBody content",
		},
		{
			name: "only frontmatter no body",
			input: "---\ndescription: Just a desc\n---\n",
			wantFM: map[string]interface{}{
				"description": "Just a desc",
			},
			wantBody: "",
		},
		{
			name:    "malformed no closing",
			input:   "---\ndescription: broken\n# no close",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := parseFrontmatter([]byte(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(fm) != len(tt.wantFM) {
				t.Fatalf("frontmatter keys mismatch: got %v, want %v", fm, tt.wantFM)
			}
			for k, v := range tt.wantFM {
				got, ok := fm[k]
				if !ok {
					t.Fatalf("missing key %q in frontmatter", k)
				}
				switch want := v.(type) {
				case []string:
					gotArr, ok := got.([]string)
					if !ok {
						t.Fatalf("key %q: got %T, want []string", k, got)
					}
					if len(gotArr) != len(want) {
						t.Fatalf("key %q: got %v, want %v", k, gotArr, want)
					}
				default:
					if got != want {
						t.Fatalf("key %q: got %v, want %v", k, got, want)
					}
				}
			}
			if body != tt.wantBody {
				t.Fatalf("body mismatch:\ngot:  %q\nwant: %q", body, tt.wantBody)
			}
		})
	}
}

func TestCursorImporter_Detect(t *testing.T) {
	t.Run("no cursor dir", func(t *testing.T) {
		withTestDir(t, func(dir string) {
			imp := NewCursorImporter(filepath.Join(dir, "nonexistent"), nil)
			if imp.Detect() {
				t.Fatal("should not detect")
			}
		})
	})

	t.Run("mdc files detected", func(t *testing.T) {
		withTestDir(t, func(dir string) {
			writeMDC(t, dir, "test.mdc", "---\ndescription: test\n---\nbody")
			imp := NewCursorImporter(dir, nil)
			if !imp.Detect() {
				t.Fatal("should detect mdc files")
			}
		})
	})

	t.Run("non-mdc files ignored", func(t *testing.T) {
		withTestDir(t, func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o755); err != nil {
				t.Fatal(err)
			}
			os.WriteFile(filepath.Join(dir, "rules", "readme.txt"), []byte("hi"), 0o644)
			imp := NewCursorImporter(dir, nil)
			if imp.Detect() {
				t.Fatal("should not detect non-mdc files")
			}
		})
	})

	t.Run("mcp.json detected", func(t *testing.T) {
		withTestDir(t, func(dir string) {
			os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{}"), 0o644)
			imp := NewCursorImporter(dir, nil)
			if !imp.Detect() {
				t.Fatal("should detect mcp.json")
			}
		})
	})
}

func TestCursorImporter_Import_MDC(t *testing.T) {
	withTestDir(t, func(dir string) {
		writeMDC(t, dir, "arch-rules.mdc", "---\ndescription: Architecture rules\nglobs: src/**\n---\n# Architecture\nUse clean patterns.")
		writeMDC(t, dir, "multi-glob.mdc", "---\ndescription: Multi rules\nglobs: [\"src/**\", \"lib/**\"]\n---\n# Multi\nRule body.")

		store := &cursorMockStore{}
		imp := NewCursorImporter(dir, nil)
		result := imp.Import(context.Background(), store)

		if result.Source != "cursor" {
			t.Fatalf("source: got %q, want %q", result.Source, "cursor")
		}
		if result.Found != 2 {
			t.Fatalf("found: got %d, want 2", result.Found)
		}
		if result.Imported != 2 {
			t.Fatalf("imported: got %d, want 2", result.Imported)
		}
		if result.Errors != 0 {
			t.Fatalf("errors: got %d, want 0", result.Errors)
		}
		if len(store.saved) != 2 {
			t.Fatalf("saved count: got %d, want 2", len(store.saved))
		}

		first := store.saved[0]
		if first.category != "preference" {
			t.Fatalf("category: got %q, want %q", first.category, "preference")
		}
		if first.source != "cursor" {
			t.Fatalf("source: got %q, want %q", first.source, "cursor")
		}
		if first.cwd != "" {
			t.Fatalf("cwd: got %q, want empty", first.cwd)
		}
		if !strings.Contains(first.content, "# arch-rules") {
			t.Fatalf("content should contain filename heading, got: %s", first.content)
		}
		if !strings.Contains(first.content, "Architecture rules") {
			t.Fatalf("content should contain description, got: %s", first.content)
		}
		if !strings.Contains(first.content, "Use clean patterns.") {
			t.Fatalf("content should contain body, got: %s", first.content)
		}

		second := store.saved[1]
		if !strings.Contains(second.content, "<!-- metadata:") {
			t.Fatalf("should have metadata comment, got: %s", second.content)
		}
		if !strings.Contains(second.content, "\"globs\":[\"src/**\",\"lib/**\"]") {
			t.Fatalf("should have globs array in metadata, got: %s", second.content)
		}
	})
}

func TestCursorImporter_Import_MalformedFrontmatter(t *testing.T) {
	withTestDir(t, func(dir string) {
		writeMDC(t, dir, "bad.mdc", "---\ndescription: no closing\nstill broken")
		writeMDC(t, dir, "good.mdc", "---\ndescription: ok\n---\nbody")

		store := &cursorMockStore{}
		imp := NewCursorImporter(dir, nil)
		result := imp.Import(context.Background(), store)

		if result.Found != 2 {
			t.Fatalf("found: got %d, want 2", result.Found)
		}
		if result.Skipped != 1 {
			t.Fatalf("skipped: got %d, want 1", result.Skipped)
		}
		if result.Imported != 1 {
			t.Fatalf("imported: got %d, want 1", result.Imported)
		}
		if result.Errors != 0 {
			t.Fatalf("errors: got %d, want 0", result.Errors)
		}
	})
}

func TestCursorImporter_Import_MCPJSON(t *testing.T) {
	withTestDir(t, func(dir string) {
		mcpContent := `{"mcpServers":{"test":{"command":"node","args":["server.js"]}}}`
		os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(mcpContent), 0o644)

		store := &cursorMockStore{}
		imp := NewCursorImporter(dir, nil)
		result := imp.Import(context.Background(), store)

		if result.Found != 1 {
			t.Fatalf("found: got %d, want 1", result.Found)
		}
		if result.Imported != 1 {
			t.Fatalf("imported: got %d, want 1", result.Imported)
		}

		if len(store.saved) != 1 {
			t.Fatalf("saved count: got %d, want 1", len(store.saved))
		}
		if store.saved[0].category != "fact" {
			t.Fatalf("category: got %q, want %q", store.saved[0].category, "fact")
		}
		if !strings.Contains(store.saved[0].content, "Cursor MCP Configuration") {
			t.Fatalf("content should have header, got: %s", store.saved[0].content)
		}
	})
}

func TestCursorImporter_Import_NoFrontmatter(t *testing.T) {
	withTestDir(t, func(dir string) {
		writeMDC(t, dir, "no-fm.mdc", "# Direct rules\nNo frontmatter here.")

		store := &cursorMockStore{}
		imp := NewCursorImporter(dir, nil)
		result := imp.Import(context.Background(), store)

		if result.Imported != 1 {
			t.Fatalf("imported: got %d, want 1 (files without frontmatter should still import)", result.Imported)
		}
		if !strings.Contains(store.saved[0].content, "# Direct rules") {
			t.Fatalf("should preserve body content, got: %s", store.saved[0].content)
		}
	})
}

func TestCursorImporter_MetadataStructure(t *testing.T) {
	withTestDir(t, func(dir string) {
		writeMDC(t, dir, "test.mdc", "---\ndescription: test\nglobs: src/**\n---\nbody")

		store := &cursorMockStore{}
		imp := NewCursorImporter(dir, nil)
		imp.Import(context.Background(), store)

		content := store.saved[0].content
		marker := "<!-- metadata: "
		idx := strings.Index(content, marker)
		if idx == -1 {
			t.Fatal("metadata comment not found")
		}
		endIdx := strings.Index(content[idx:], " -->")
		jsonStr := content[idx+len(marker) : idx+endIdx]
		jsonStr = strings.TrimSpace(jsonStr)

		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &meta); err != nil {
			t.Fatalf("metadata not valid JSON: %v", err)
		}
		if meta["file"] != "test.mdc" {
			t.Fatalf("metadata file: got %v, want test.mdc", meta["file"])
		}
		if meta["globs"] != "src/**" {
			t.Fatalf("metadata globs: got %v, want src/**", meta["globs"])
		}
	})
}
