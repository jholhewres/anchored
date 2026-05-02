package ctx

import (
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type ChunkData struct {
	Heading string
	Content string
	HasCode bool
}

type Chunker struct {
	maxBytes int
}
func NewChunker(maxBytes int) *Chunker {
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	return &Chunker{maxBytes: maxBytes}
}

func (c *Chunker) Chunk(source []byte) []ChunkData {
	if len(source) == 0 {
		return nil
	}

	root := goldmark.New().Parser().Parse(text.NewReader(source))

	type chunk struct {
		heading  string
		content  strings.Builder
		hasCode  bool
	}

	var chunks []*chunk
	var headingStack []string
	current := &chunk{}

	flush := func() {
		if current.content.Len() == 0 {
			return
		}
		chunks = append(chunks, current)
		current = &chunk{}
	}

	// headingBreadcrumb builds the breadcrumb from the heading stack,
	// trimming deeper headings at the same or lower level.
	updateHeadingStack := func(level int, text string) {
		for len(headingStack) >= level {
			headingStack = headingStack[:len(headingStack)-1]
		}
		headingStack = append(headingStack, text)
	}

	getBreadcrumb := func() string {
		return strings.Join(headingStack, " > ")
	}

	ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch node := n.(type) {
		case *ast.Heading:
			if entering {
				flush()
				headingText := strings.TrimSpace(string(node.Text(source)))
				updateHeadingStack(node.Level, headingText)
				current.heading = getBreadcrumb()
			}
			return ast.WalkContinue, nil

		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if entering {
				current.hasCode = true
				codeText := extractCodeBlock(n, source)
				if current.content.Len() > 0 {
					current.content.WriteString("\n\n")
				}
				current.content.WriteString(codeText)
			}
			return ast.WalkSkipChildren, nil

		case *ast.Paragraph:
			if entering {
				text := strings.TrimSpace(string(node.Text(source)))
				if text == "" {
					return ast.WalkContinue, nil
				}
				if current.content.Len() > 0 {
					current.content.WriteString("\n\n")
				}
				current.content.WriteString(text)
			}
			return ast.WalkContinue, nil

		case *ast.List:
			if entering {
				items := extractListItems(node, source)
				if items == "" {
					return ast.WalkContinue, nil
				}
				if current.content.Len() > 0 {
					current.content.WriteString("\n\n")
				}
				current.content.WriteString(items)
				return ast.WalkSkipChildren, nil
			}
			return ast.WalkContinue, nil

		case *ast.Blockquote:
			if entering {
				text := strings.TrimSpace(string(node.Text(source)))
				if text == "" {
					return ast.WalkContinue, nil
				}
				if current.content.Len() > 0 {
					current.content.WriteString("\n\n")
				}
				current.content.WriteString(text)
				return ast.WalkSkipChildren, nil
			}
			return ast.WalkContinue, nil

		case *ast.ThematicBreak:
			if entering {
				flush()
			}
			return ast.WalkContinue, nil
		}

		return ast.WalkContinue, nil
	})

	flush()

	if len(chunks) == 0 {
		return nil
	}

	var result []ChunkData
	for _, ch := range chunks {
		content := ch.content.String()
		if len(content) == 0 {
			continue
		}
		if len(content) <= c.maxBytes || ch.hasCode {
			result = append(result, ChunkData{
				Heading: ch.heading,
				Content: content,
				HasCode: ch.hasCode,
			})
			continue
		}
		parts := splitOversized(content, c.maxBytes)
		for _, part := range parts {
			result = append(result, ChunkData{
				Heading: ch.heading,
				Content: part,
				HasCode: false,
			})
		}
	}

	return result
}

func extractCodeBlock(node ast.Node, source []byte) string {
	var sb strings.Builder

	if fenced, ok := node.(*ast.FencedCodeBlock); ok {
		lang := fenced.Language(source)
		if lang != nil {
			sb.WriteString("```")
			sb.WriteString(string(lang))
			sb.WriteString("\n")
		} else {
			sb.WriteString("```\n")
		}
		for i := 0; i < fenced.Lines().Len(); i++ {
			line := fenced.Lines().At(i)
			sb.Write(line.Value(source))
		}
		sb.WriteString("```")
		return sb.String()
	}

	for i := 0; i < node.Lines().Len(); i++ {
		line := node.Lines().At(i)
		sb.Write(line.Value(source))
	}
	return strings.TrimSpace(sb.String())
}

func extractListItems(list *ast.List, source []byte) string {
	var sb strings.Builder
	idx := 1
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		li, ok := child.(*ast.ListItem)
		if !ok {
			continue
		}
		for sub := li.FirstChild(); sub != nil; sub = sub.NextSibling() {
			text := strings.TrimSpace(string(sub.Text(source)))
			if text == "" {
				continue
			}
			if list.IsOrdered() {
				sb.WriteString(strconv.Itoa(idx) + ". ")
			} else {
				sb.WriteString("- ")
			}
			sb.WriteString(text)
			sb.WriteString("\n")
		}
		idx++
	}
	return strings.TrimSpace(sb.String())
}

func splitOversized(content string, maxBytes int) []string {
	paragraphs := strings.Split(content, "\n\n")

	var chunks []string
	var current strings.Builder

	for _, para := range paragraphs {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}

		if len(para) > maxBytes {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
			}
			chunks = append(chunks, hardSplit(para, maxBytes)...)
			continue
		}

		if current.Len() > 0 && current.Len()+2+len(para) > maxBytes {
			chunks = append(chunks, current.String())
			current.Reset()
		}

		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(para)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}

	return chunks
}

func hardSplit(s string, maxBytes int) []string {
	if len(s) <= maxBytes {
		return []string{s}
	}

	var parts []string
	for len(s) > maxBytes {
		cut := maxBytes
		if idx := strings.LastIndex(s[:cut], "\n"); idx > 0 {
			cut = idx + 1
		} else if idx := strings.LastIndex(s[:cut], " "); idx > 0 {
			cut = idx + 1
		}
		parts = append(parts, strings.TrimSpace(s[:cut]))
		s = s[cut:]
	}
	if len(s) > 0 {
		parts = append(parts, strings.TrimSpace(s))
	}
	return parts
}
