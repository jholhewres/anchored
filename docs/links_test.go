package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// linkRe captures a link's target: a Markdown link "](target)", or an HTML
// src="target" or href="target" (the READMEs' badges and language switcher).
var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)|src="([^"]+)"|href="([^"]+)"`)

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
			target := m[1] + m[2] + m[3]
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

// TestContributingCarriesTheReleaseSteps locks what the README cut moved into
// CONTRIBUTING.md: the release steps, with the wait for CI on Linux and macOS
// before tagging, and the pointer to docs/setup.md for a new tool.
func TestContributingCarriesTheReleaseSteps(t *testing.T) {
	data, err := os.ReadFile("../CONTRIBUTING.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	_, releasing, ok := strings.Cut(text, "\n## Releasing\n")
	if !ok {
		t.Fatal("CONTRIBUTING.md has no Releasing section")
	}
	releasing, _, _ = strings.Cut(releasing, "\n## ")
	for _, want := range []string{"VERSION", "make sync-version", "CHANGELOG.md", "Linux", "macOS", "before", "tag"} {
		if !strings.Contains(releasing, want) {
			t.Errorf("the Releasing section does not mention %q", want)
		}
	}
	_, tools, ok := strings.Cut(text, "\n## Adding Tool Support\n")
	if !ok {
		t.Fatal("CONTRIBUTING.md has no Adding Tool Support section")
	}
	tools, _, _ = strings.Cut(tools, "\n## ")
	if !strings.Contains(tools, "docs/setup.md") || strings.Contains(tools, "README.md") {
		t.Error("Adding Tool Support must send tool authors to docs/setup.md, not the README")
	}
}
