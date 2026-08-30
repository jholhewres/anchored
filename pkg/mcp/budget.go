package mcp

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Response budgets. Claude Code persists an oversized tool result to a file
// on disk and leaves only a stub in context — the agent then re-reads the
// file in slices (burning the tokens this plugin exists to save) or simply
// stops calling the tool. Keeping every response comfortably under that
// persistence threshold is a product requirement, so every output that
// scales with stored or executed content is bounded here.
const (
	// searchHitRunes caps one hit's content in anchored_search output.
	searchHitRunes = 700
	// searchHitRunesFull is the per-hit cap with full=true — generous, but
	// still bounded so one 100KB memory can't flood the reply.
	searchHitRunesFull = 4096
	// searchBudgetBytes caps the whole anchored_search response.
	searchBudgetBytes = 8 * 1024
	// searchBudgetBytesFull is the response cap with full=true.
	searchBudgetBytesFull = 24 * 1024
	// listItemRunes caps one memory's content in anchored_list output.
	listItemRunes = 700
	// skillSearchLimit keeps remote instruction discovery compact. Full bodies
	// are intentionally available only through anchored_skill(action="load").
	skillSearchLimit = 12
	// skillLoadBudgetBytes caps a loaded skill response so a malformed or
	// unexpectedly large remote body cannot consume the entire tool context.
	skillLoadBudgetBytes = 24 * 1024
	// skillLoadContentRunes reserves space for the explicit instruction fence,
	// escaped Markdown, and provenance metadata inside skillLoadBudgetBytes.
	skillLoadContentRunes = 3000
	// execInlineBytes is the largest execution output returned inline;
	// anything bigger is indexed and answered with a head/tail preview.
	execInlineBytes = 8 * 1024
	// execPreviewHead/Tail shape the preview of an over-budget execution.
	execPreviewHead = 4 * 1024
	execPreviewTail = 2 * 1024
	// execErrHead/Tail cap stderr echoed back from a failed execution.
	execErrHead = 1536
	execErrTail = 512
)

// defaultCWD substitutes the server process's working directory for an
// omitted cwd. Claude Code launches the MCP server in the project directory,
// so "." is the right project for every automatic flow (remote merge, save
// write-through, kg push) — an empty cwd must not silently mean "no team
// memory". Explicit cwd always wins.
func defaultCWD(cwd string) string {
	if cwd == "" {
		return "."
	}
	return cwd
}

// headTail keeps the first `head` and last `tail` bytes of s — realigned to
// rune boundaries so multibyte text never yields a torn character — with an
// omission marker in between. Strings that already fit pass through.
func headTail(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	h := s[:head]
	for len(h) > 0 {
		if r, size := utf8.DecodeLastRuneInString(h); r == utf8.RuneError && size <= 1 {
			h = h[:len(h)-1]
			continue
		}
		break
	}
	t := s[len(s)-tail:]
	for len(t) > 0 {
		if r, size := utf8.DecodeRuneInString(t); r == utf8.RuneError && size <= 1 {
			t = t[1:]
			continue
		}
		break
	}
	return h + fmt.Sprintf("\n[... %d bytes omitted ...]\n", len(s)-len(h)-len(t)) + t
}

// searchHitWriter renders <hit> entries under a per-hit rune cap and a
// whole-response byte budget. Hits past the budget are counted, not rendered,
// and surface as an <omitted> note so the agent knows to refine instead of
// assuming it saw everything.
type searchHitWriter struct {
	sb       strings.Builder
	hitRunes int
	budget   int
	omitted  int
}

func newSearchHitWriter(full bool) *searchHitWriter {
	if full {
		return &searchHitWriter{hitRunes: searchHitRunesFull, budget: searchBudgetBytesFull}
	}
	return &searchHitWriter{hitRunes: searchHitRunes, budget: searchBudgetBytes}
}

// open writes the envelope's opening tag (shape varies per search path).
func (w *searchHitWriter) open(format string, args ...any) {
	fmt.Fprintf(&w.sb, format, args...)
}

// hit appends one result, flattened to a single line and capped.
func (w *searchHitWriter) hit(attrs []string, content string) {
	content = strings.ReplaceAll(content, "\n", " ")
	content = strings.ReplaceAll(content, "\r", " ")
	content = truncateRunes(content, w.hitRunes)
	line := "  <hit " + strings.Join(attrs, " ") + ">" + escapeText(content) + "</hit>\n"
	if w.sb.Len()+len(line) > w.budget {
		w.omitted++
		return
	}
	w.sb.WriteString(line)
}

// close emits the omitted note (if any) and the closing tag.
func (w *searchHitWriter) close() string {
	if w.omitted > 0 {
		fmt.Fprintf(&w.sb,
			"  <omitted count=\"%d\" hint=\"response budget reached — narrow the query, lower max_results, or read one memory with full=true\"/>\n",
			w.omitted)
	}
	w.sb.WriteString("</anchored_search>")
	return w.sb.String()
}
