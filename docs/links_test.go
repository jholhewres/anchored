package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)|src="([^"]+)"`)

// TestDocsLinksResolve locks the documentation's own links: every relative
// link in the READMEs, CONTRIBUTING, the glossary, the CHANGELOG and
// docs/*.md points at a file inside the repository that exists, and every
// page in docs/ is linked from the README, so a page that moves or loses its
// last link is caught. A link out of the repository resolves only on a
// machine that happens to hold the sibling checkout.
func TestDocsLinksResolve(t *testing.T) {
	pages, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatal(err)
	}
	files := append([]string{"../README.md", "../README.pt-BR.md", "../CONTRIBUTING.md", "../GLOSSARY.md", "../CHANGELOG.md"}, pages...)
	linked := map[string]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(data), -1) {
			target := m[1] + m[2]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			resolved := filepath.Clean(filepath.Join(filepath.Dir(file), target))
			if strings.HasPrefix(resolved, "../..") {
				t.Errorf("%s links to %s, outside the repository: broken for anyone reading it on GitHub", file, target)
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s links to %s, which does not exist", file, target)
			}
			if file == "../README.md" {
				linked[resolved] = true
			}
		}
	}
	for _, page := range pages {
		if !linked[filepath.Clean(filepath.Join("..", "docs", page))] {
			t.Errorf("docs/%s is not linked from README.md", page)
		}
	}
}
