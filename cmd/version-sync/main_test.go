package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRelease(t *testing.T, version, plugin, marketplaceMeta, marketplacePlugin string) string {
	t.Helper()
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, ".claude-plugin"), 0o755))
	must(os.WriteFile(filepath.Join(root, "VERSION"), []byte(version+"\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"anchored","version":"`+plugin+`"}`), 0o644))
	must(os.WriteFile(filepath.Join(root, ".claude-plugin", "marketplace.json"),
		[]byte(`{"metadata":{"version":"`+marketplaceMeta+`"},"plugins":[{"name":"anchored","version":"`+marketplacePlugin+`"}]}`), 0o644))
	return root
}

func TestCheck_PassesWhenEverythingAgrees(t *testing.T) {
	root := writeRelease(t, "0.20.0", "0.20.0", "0.20.0", "0.20.0")
	if err := run(root, true, "v0.20.0"); err != nil {
		t.Fatalf("aligned release must pass: %v", err)
	}
	if err := run(root, true, ""); err != nil {
		t.Fatalf("aligned manifests without a tag must pass: %v", err)
	}
}

// The release workflow runs the check before building anything: a tag that
// disagrees with VERSION, or a manifest that was never synced, would publish
// binaries and a plugin that report different versions.
func TestCheck_FailsOnAnyDisagreement(t *testing.T) {
	cases := map[string]struct {
		root, tag, want string
	}{
		"tag":         {writeRelease(t, "0.20.0", "0.20.0", "0.20.0", "0.20.0"), "v0.19.2", "tag"},
		"plugin":      {writeRelease(t, "0.20.0", "0.19.2", "0.20.0", "0.20.0"), "", "plugin.json"},
		"marketplace": {writeRelease(t, "0.20.0", "0.20.0", "0.20.0", "0.19.2"), "", "marketplace.json"},
	}
	for name, c := range cases {
		err := run(c.root, true, c.tag)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: expected an error naming %q, got %v", name, c.want, err)
		}
	}
}

func TestCheck_WritesNothing(t *testing.T) {
	root := writeRelease(t, "0.20.0", "0.19.2", "0.19.2", "0.19.2")
	before, _ := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	_ = run(root, true, "")
	after, _ := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if string(before) != string(after) {
		t.Error("--check must not rewrite the manifests")
	}
}

func TestSync_RewritesManifests(t *testing.T) {
	root := writeRelease(t, "0.20.0", "0.19.2", "0.19.2", "0.19.2")
	if err := run(root, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := run(root, true, "v0.20.0"); err != nil {
		t.Errorf("after a sync the check must pass: %v", err)
	}
}

func TestCheck_CatchesMarketplaceMetadataDrift(t *testing.T) {
	root := writeRelease(t, "0.20.0", "0.20.0", "0.19.2", "0.20.0")
	if err := run(root, true, ""); err == nil || !strings.Contains(err.Error(), "metadata.version") {
		t.Errorf("metadata.version drift must be reported, got %v", err)
	}
}
