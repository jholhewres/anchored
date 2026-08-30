package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hooks/hooks.json is the plugin manifest Claude Code installs. Its matchers
// are regexes matched (unanchored) against the wire tool name, and the wire
// name carries a server prefix that DRIFTS between harnesses: the same tool
// registers as mcp__anchored__anchored_context in one and as
// mcp__plugin_anchored_anchored__anchored_context in another. A matcher that
// hardcodes one prefix therefore stops firing with no error anywhere — the
// hook simply never runs. That is exactly what happened to the PostToolUse
// entry: "mcp__anchored__.*" never matched under the plugin-scoped prefix, so
// artifact capture, working-set feed, session-event timeline and the redundant
// context-gate credit were all silently dead for every anchored MCP call.
//
// This test pins the invariant that no manifest matcher may depend on a
// specific registration prefix.

type hooksManifest struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
	} `json:"hooks"`
}

// anchoredToolNames are the same tool under every registration prefix a
// harness has been observed to use. A matcher meant for anchored's own MCP
// tools must match all of them.
var anchoredToolNames = []string{
	"mcp__anchored__anchored_context",
	"mcp__plugin_anchored_anchored__anchored_context",
	"mcp__plugin_anchored_anchored__anchored_search",
}

// foreignToolNames must NOT match anchored's MCP matcher: the hook shells out
// a binary per call, so matching every third-party MCP tool is a real cost.
// The last three are the near-misses that a loose "contains anchored" matcher
// would wrongly catch — including anchored-work, this workspace's own sibling
// project, which would start paying for anchored's hook the day it registers an
// MCP server.
var foreignToolNames = []string{
	"mcp__chrome-devtools__click",
	"mcp__plugin_playwright_playwright__browser_click",
	"mcp__sonar-report__reports_create",
	"mcp__anchored-work__list_tasks",
	"mcp__unanchored__query",
	"mcp__notion__get_anchored_page",
}

func loadHooksManifest(t *testing.T) hooksManifest {
	t.Helper()
	path := filepath.Join("..", "..", "hooks", "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m hooksManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(m.Hooks) == 0 {
		t.Fatalf("%s declares no hooks", path)
	}
	return m
}

// TestHooksManifestMCPMatchersArePrefixAgnostic asserts that for every event
// that wires an MCP-targeting matcher, at least one matcher fires for anchored
// tools under EVERY known registration prefix.
func TestHooksManifestMCPMatchersArePrefixAgnostic(t *testing.T) {
	m := loadHooksManifest(t)

	for event, groups := range m.Hooks {
		var mcpMatchers []*regexp.Regexp
		for _, g := range groups {
			if g.Matcher == "" || !strings.Contains(g.Matcher, "mcp__") {
				continue
			}
			re, err := regexp.Compile(g.Matcher)
			if err != nil {
				t.Fatalf("%s: matcher %q does not compile: %v", event, g.Matcher, err)
			}
			mcpMatchers = append(mcpMatchers, re)
		}
		if len(mcpMatchers) == 0 {
			if requiresMCPMatcher[event] {
				t.Errorf("%s: no MCP matcher at all — anchored's own tools would silently stop reaching this hook", event)
			}
			continue
		}
		for _, tool := range anchoredToolNames {
			matched := false
			for _, re := range mcpMatchers {
				if re.MatchString(tool) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("%s: no matcher fires for %q — the hook is silently dead under this registration prefix", event, tool)
			}
		}
	}
}

// requiresMCPMatcher lists the events that MUST keep an MCP-targeting matcher.
// A drifted matcher and a DELETED matcher are the same silent death — the hook
// simply stops running — so the prefix-agnostic test must fail on both, not
// skip the event when no matcher is left to check.
var requiresMCPMatcher = map[string]bool{"PreToolUse": true, "PostToolUse": true}

// catchAllMCPEvents are the events whose MCP matcher is DELIBERATELY a
// catch-all. PreToolUse hosts the context gate, which must observe every tool
// call — an anchored-scoped matcher there would let an agent walk past the
// gate by reaching for any third-party MCP tool first. Every other event is
// anchored-scoped and pays a binary exec per matched call, so breadth there is
// pure cost.
var catchAllMCPEvents = map[string]bool{"PreToolUse": true}

// TestHooksManifestMCPMatchersDoNotCatchForeignServers guards the other side:
// prefix-agnostic must not mean "every MCP tool on the machine".
func TestHooksManifestMCPMatchersDoNotCatchForeignServers(t *testing.T) {
	m := loadHooksManifest(t)

	for event, groups := range m.Hooks {
		if catchAllMCPEvents[event] {
			continue
		}
		for _, g := range groups {
			if g.Matcher == "" || !strings.Contains(g.Matcher, "mcp__") {
				continue
			}
			re := regexp.MustCompile(g.Matcher)
			for _, tool := range foreignToolNames {
				if re.MatchString(tool) {
					t.Errorf("%s: matcher %q also fires for unrelated server tool %q", event, g.Matcher, tool)
				}
			}
		}
	}
}
