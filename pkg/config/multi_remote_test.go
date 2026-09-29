package config

import "testing"

// TestMigrateRemotes_MergesSingularIntoMap locks the multi-server contract:
// the legacy singular `remote:` block must keep resolving as "default" even
// when named `remotes:` entries exist — otherwise adding a company server
// would silently drop the personal one from routing.
func TestMigrateRemotes_MergesSingularIntoMap(t *testing.T) {
	cfg := Defaults()
	cfg.Remote.Enabled = true
	cfg.Remote.ServerURL = "https://personal.example"
	cfg.Remote.APIKey = "k1"
	cfg.Remote.Projects = []string{"proj-1"}
	cfg.Remotes = map[string]RemoteEntry{
		"company": {ServerURL: "https://company.example", APIKey: "k2", Paths: []string{"/home/me/work/*"}},
	}

	cfg.migrateRemotes()

	def, ok := cfg.Remotes["default"]
	if !ok {
		t.Fatal("singular remote was not merged into the map as \"default\"")
	}
	if !def.Default {
		t.Error("merged default entry must carry Default=true when no other entry has it")
	}
	if def.ServerURL != "https://personal.example" || len(def.Projects) != 1 {
		t.Errorf("merged default lost fields: %+v", def)
	}
	if cfg.Remotes["company"].Name != "company" {
		t.Errorf("named entry Name not normalized: %+v", cfg.Remotes["company"])
	}
}

// TestResolveRemote_PathsRouteAndDefaultFallback verifies routing: paths
// globs pick the matching remote, everything else falls back to the default.
// Projects (linked IDs) must NOT participate in routing.
func TestResolveRemote_PathsRouteAndDefaultFallback(t *testing.T) {
	cfg := Defaults()
	cfg.Remotes = map[string]RemoteEntry{
		"default": {Name: "default", ServerURL: "https://personal.example", Default: true},
		"company": {
			Name:      "company",
			ServerURL: "https://company.example",
			Paths:     []string{"/home/me/work/*"},
			Projects:  []string{"/home/me/oss/repo"}, // a linked ID that looks like a path — must not route
		},
	}

	if e := cfg.ResolveRemote("/home/me/work/api"); e == nil || e.Name != "company" {
		t.Errorf("work path should route to company, got %+v", e)
	}
	if e := cfg.ResolveRemote("/home/me/oss/repo"); e == nil || e.Name != "default" {
		t.Errorf("projects (linked IDs) must not route; expected default, got %+v", e)
	}
	if e := cfg.ResolveRemote("/anywhere/else"); e == nil || e.Name != "default" {
		t.Errorf("unmatched path should fall back to default, got %+v", e)
	}
}

// A singular block turned off (`remote configure --disable`) stays out of the
// map: `remote status` reports it disabled, so it must not keep routing
// searches and syncs to its server.
func TestMigrateRemotes_SkipsADisabledSingularBlock(t *testing.T) {
	cfg := Defaults()
	cfg.Remote.Enabled = false
	cfg.Remote.ServerURL = "https://gone.example"
	cfg.Remote.APIKey = "k1"
	cfg.Remotes = map[string]RemoteEntry{
		"company": {ServerURL: "https://company.example", APIKey: "k2"},
	}

	cfg.migrateRemotes()

	if _, ok := cfg.Remotes["default"]; ok {
		t.Fatal("a disabled singular remote was merged into routing")
	}
	if cfg.ResolveRemote("/anywhere") != nil {
		t.Fatal("a disabled default remote still resolves")
	}
	if _, ok := cfg.Remotes["company"]; !ok {
		t.Fatal("named remotes must be unaffected")
	}
}
