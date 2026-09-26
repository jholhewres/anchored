// Command version-sync rewrites the plugin manifests so they always agree
// with the canonical version in /VERSION.
//
// Run via `make sync-version` after bumping VERSION. The tool is intentionally
// dumb: it loads each JSON file, rewrites the version-bearing fields, and
// writes back with the same indentation. Goreleaser already picks the version
// from the git tag (which should match VERSION) so it doesn't need rewriting.
//
// With --check it rewrites nothing and fails when a manifest disagrees with
// VERSION or, given --tag, when the release tag does: the release workflow
// runs it before building so binaries and plugin never ship different
// versions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	check := flag.Bool("check", false, "verify the manifests (and --tag) match VERSION; write nothing")
	tag := flag.String("tag", "", "release tag that must match VERSION (e.g. v0.20.0)")
	flag.Parse()
	if *tag != "" && !*check {
		fail("--tag only applies with --check")
	}
	if err := run(".", *check, *tag); err != nil {
		fail("%v", err)
	}
}

func run(root string, check bool, tag string) error {
	version, err := readVersion(filepath.Join(root, "VERSION"))
	if err != nil {
		return fmt.Errorf("read VERSION: %w", err)
	}
	pluginPath := filepath.Join(root, ".claude-plugin", "plugin.json")
	marketplacePath := filepath.Join(root, ".claude-plugin", "marketplace.json")

	if check {
		var problems []string
		if tag != "" && strings.TrimPrefix(tag, "v") != version {
			problems = append(problems, fmt.Sprintf("tag %s != VERSION %s", tag, version))
		}
		found, err := manifestVersions(pluginPath, marketplacePath)
		if err != nil {
			return err
		}
		for _, f := range found {
			if f.version != version {
				problems = append(problems, fmt.Sprintf("%s %s is %q, VERSION is %s", f.file, f.field, f.version, version))
			}
		}
		if len(problems) > 0 {
			return fmt.Errorf("version mismatch (run `make sync-version`):\n  %s", strings.Join(problems, "\n  "))
		}
		fmt.Printf("versions agree: v%s\n", version)
		return nil
	}

	if err := syncPluginJSON(pluginPath, version); err != nil {
		return fmt.Errorf("sync plugin.json: %w", err)
	}
	if err := syncMarketplaceJSON(marketplacePath, version); err != nil {
		return fmt.Errorf("sync marketplace.json: %w", err)
	}
	fmt.Printf("synced manifests to v%s\n", version)
	return nil
}

type manifestVersion struct{ file, field, version string }

// manifestVersions reads every version-bearing field syncPluginJSON and
// syncMarketplaceJSON write.
func manifestVersions(pluginPath, marketplacePath string) ([]manifestVersion, error) {
	var out []manifestVersion
	var plugin map[string]any
	if err := readJSON(pluginPath, &plugin); err != nil {
		return nil, err
	}
	v, _ := plugin["version"].(string)
	out = append(out, manifestVersion{"plugin.json", "version", v})

	var market map[string]any
	if err := readJSON(marketplacePath, &market); err != nil {
		return nil, err
	}
	if md, ok := market["metadata"].(map[string]any); ok {
		v, _ := md["version"].(string)
		out = append(out, manifestVersion{"marketplace.json", "metadata.version", v})
	}
	if plugins, ok := market["plugins"].([]any); ok {
		for i, p := range plugins {
			if pm, ok := p.(map[string]any); ok {
				v, _ := pm["version"].(string)
				out = append(out, manifestVersion{"marketplace.json", fmt.Sprintf("plugins[%d].version", i), v})
			}
		}
	}
	return out, nil
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("VERSION is empty")
	}
	if strings.HasPrefix(v, "v") {
		v = v[1:]
	}
	return v, nil
}

// syncPluginJSON updates the top-level "version" field in plugin.json. The
// file is small and hand-authored, so we round-trip through a generic map and
// rewrite with the same 2-space indent the file ships with.
func syncPluginJSON(path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	doc["version"] = version
	return writeJSON(path, doc)
}

// syncMarketplaceJSON updates BOTH metadata.version and plugins[*].version
// (where the inner plugin name matches the manifest "name"), since the
// marketplace format duplicates the version in two places.
func syncMarketplaceJSON(path, version string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if md, ok := doc["metadata"].(map[string]any); ok {
		md["version"] = version
	}
	if plugins, ok := doc["plugins"].([]any); ok {
		for _, p := range plugins {
			if pm, ok := p.(map[string]any); ok {
				pm["version"] = version
			}
		}
	}
	return writeJSON(path, doc)
}

func writeJSON(path string, doc any) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "version-sync: "+format+"\n", args...)
	os.Exit(1)
}
