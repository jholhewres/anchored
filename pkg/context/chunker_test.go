package ctx

import (
	"strings"
	"testing"
)

func TestChunker_HeadingSplit(t *testing.T) {
	md := `# Title 1

Paragraph one.

## Section A

Paragraph two.

# Title 2

Paragraph three.`

	c := NewChunker(4096)
	chunks := c.Chunk([]byte(md))

	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}

	if chunks[0].Heading != "Title 1" {
		t.Errorf("chunk[0].Heading = %q, want %q", chunks[0].Heading, "Title 1")
	}
	if !strings.Contains(chunks[0].Content, "Paragraph one.") {
		t.Errorf("chunk[0].Content missing expected text: %q", chunks[0].Content)
	}

	if chunks[1].Heading != "Title 1 > Section A" {
		t.Errorf("chunk[1].Heading = %q, want %q", chunks[1].Heading, "Title 1 > Section A")
	}

	if chunks[2].Heading != "Title 2" {
		t.Errorf("chunk[2].Heading = %q, want %q", chunks[2].Heading, "Title 2")
	}
}

func TestChunker_CodeBlockAtomic(t *testing.T) {
	md := `## Example

` + "```go" + `
func main() {
    fmt.Println("hello")
}
` + "```" + `

Some text after.`

	c := NewChunker(4096)
	chunks := c.Chunk([]byte(md))

	foundCode := false
	for _, ch := range chunks {
		if ch.HasCode {
			foundCode = true
			if !strings.Contains(ch.Content, "func main()") {
				t.Errorf("code chunk missing function body: %q", ch.Content)
			}
			if !strings.Contains(ch.Content, "```go") {
				t.Errorf("code chunk missing fence: %q", ch.Content)
			}
		}
	}
	if !foundCode {
		t.Fatal("no chunk with HasCode=true found")
	}
}

func TestChunker_OversizedSplit(t *testing.T) {
	paragraph := strings.Repeat("word ", 2000)
	md := "# Big\n\n" + paragraph

	c := NewChunker(512)
	chunks := c.Chunk([]byte(md))

	if len(chunks) < 2 {
		t.Fatalf("expected >= 2 chunks for oversized content, got %d", len(chunks))
	}

	for i, ch := range chunks {
		if len(ch.Content) > 600 {
			t.Errorf("chunk[%d] too large: %d bytes", i, len(ch.Content))
		}
		if ch.Heading != "Big" {
			t.Errorf("chunk[%d].Heading = %q, want %q", i, ch.Heading, "Big")
		}
	}
}

func TestChunker_EmptyInput(t *testing.T) {
	c := NewChunker(4096)

	chunks := c.Chunk([]byte{})
	if chunks != nil {
		t.Errorf("expected nil for empty input, got %v", chunks)
	}

	chunks = c.Chunk(nil)
	if chunks != nil {
		t.Errorf("expected nil for nil input, got %v", chunks)
	}

	chunks = c.Chunk([]byte("   \n\n  \t\n"))
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks for whitespace-only input, got %d", len(chunks))
	}
}

func TestChunker_NoHeadings(t *testing.T) {
	md := "Just some text without any headings at all."

	c := NewChunker(4096)
	chunks := c.Chunk([]byte(md))

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Content != md {
		t.Errorf("chunk content = %q, want %q", chunks[0].Content, md)
	}
	if chunks[0].Heading != "" {
		t.Errorf("chunk heading should be empty, got %q", chunks[0].Heading)
	}
	if chunks[0].HasCode {
		t.Error("chunk should not have HasCode=true")
	}
}

func TestChunker_NestedHeadings(t *testing.T) {
	md := `# Level One

Text one.

## Level Two

Text two.

### Level Three

Text three.

## Another Two

Text two again.

# Second Top Level

Text top.`

	c := NewChunker(4096)
	chunks := c.Chunk([]byte(md))

	if len(chunks) != 5 {
		t.Fatalf("expected 5 chunks, got %d", len(chunks))
	}

	wants := []struct {
		heading string
		content string
	}{
		{"Level One", "Text one."},
		{"Level One > Level Two", "Text two."},
		{"Level One > Level Two > Level Three", "Text three."},
		{"Level One > Another Two", "Text two again."},
		{"Second Top Level", "Text top."},
	}

	for i, w := range wants {
		if chunks[i].Heading != w.heading {
			t.Errorf("chunk[%d].Heading = %q, want %q", i, chunks[i].Heading, w.heading)
		}
		if !strings.Contains(chunks[i].Content, w.content) {
			t.Errorf("chunk[%d].Content missing %q: %q", i, w.content, chunks[i].Content)
		}
	}
}

func TestChunker_UnicodeContent(t *testing.T) {
	md := `# Título 🚀

Texto com acentos: áéíóú ñ

## Seção 2

Conteúdo com emoji: 🎉🔥✨`

	c := NewChunker(4096)
	chunks := c.Chunk([]byte(md))

	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0].Heading, "Título") {
		t.Errorf("chunk[0].Heading = %q", chunks[0].Heading)
	}
	if !strings.Contains(chunks[0].Content, "áéíóú") {
		t.Errorf("chunk[0] missing unicode: %q", chunks[0].Content)
	}
	if !strings.Contains(chunks[1].Heading, "Seção 2") {
		t.Errorf("chunk[1].Heading = %q", chunks[1].Heading)
	}
	if !strings.Contains(chunks[1].Content, "🎉") {
		t.Errorf("chunk[1] missing emoji: %q", chunks[1].Content)
	}
}

func TestChunker_CodeBlockNeverSplit(t *testing.T) {
	longLine := strings.Repeat("x", 300)
	codeBlock := "```go\n" + longLine + "\n" + longLine + "\n" + longLine + "\n```"
	md := "## Code\n\n" + codeBlock

	c := NewChunker(256)
	chunks := c.Chunk([]byte(md))

	codeChunks := 0
	for _, ch := range chunks {
		if ch.HasCode {
			codeChunks++
			if !strings.Contains(ch.Content, "```go") || !strings.Contains(ch.Content, "```") {
				t.Errorf("code block was split or malformed: %q", ch.Content)
			}
		}
	}
	if codeChunks != 1 {
		t.Errorf("expected exactly 1 code chunk, got %d", codeChunks)
	}
}

func TestChunker_DefaultMaxBytes(t *testing.T) {
	c := NewChunker(0)
	if c.maxBytes != 4096 {
		t.Errorf("default maxBytes = %d, want 4096", c.maxBytes)
	}

	c = NewChunker(-1)
	if c.maxBytes != 4096 {
		t.Errorf("default maxBytes = %d, want 4096", c.maxBytes)
	}
}
