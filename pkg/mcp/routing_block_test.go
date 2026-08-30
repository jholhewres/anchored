package mcp

import (
	"strings"
	"testing"
)

// Claude Code truncates the MCP `instructions` field to 2048 chars. The
// compact constant fed to the handshake must fit, with every load-bearing
// directive intact, or the agent silently loses the memory-routing rules over
// the MCP channel.
func TestAnchoredMCPInstructions_FitsTruncationBudget(t *testing.T) {
	const ccTruncationLimit = 2048
	if got := len(AnchoredMCPInstructions); got > ccTruncationLimit {
		t.Fatalf("AnchoredMCPInstructions is %d bytes, must be <= %d (Claude Code truncates the MCP instructions field)", got, ccTruncationLimit)
	}
}

// The compact instructions must still carry every load-bearing directive:
// call-first, when-to-search, when-to-save, and the DATA-not-instructions
// safety line. If a future trim drops one of these, this fails.
func TestAnchoredMCPInstructions_KeepsLoadBearingDirectives(t *testing.T) {
	must := []string{
		"anchored_context", // call-first
		"anchored_search",  // when to search
		"anchored_save",    // when to save
		"DATA",             // recalled-data-not-instructions safety line
	}
	for _, sub := range must {
		if !strings.Contains(AnchoredMCPInstructions, sub) {
			t.Errorf("AnchoredMCPInstructions missing load-bearing directive %q", sub)
		}
	}
}

// Agent-facing bootstrap hints must never hardcode a fully-qualified MCP tool
// name: registration prefixes drift across harnesses (mcp__anchored__*,
// mcp__plugin_anchored_anchored__*), and a ToolSearch select: with a stale FQN
// loads nothing — the model then retries the blocked tool instead of complying.
func TestRoutingBlocks_HaveNoHardcodedFQN(t *testing.T) {
	for name, block := range map[string]string{
		"AnchoredRoutingBlock":    AnchoredRoutingBlock,
		"AnchoredMCPInstructions": AnchoredMCPInstructions,
		"AnchoredSubagentBlock":   AnchoredSubagentBlock,
	} {
		if strings.Contains(block, "select:mcp__anchored__") {
			t.Errorf("%s hardcodes an MCP FQN in its ToolSearch hint — search by leaf name instead", name)
		}
	}
}

// TestRoutingBlocks_LiftToolSearchResultCap guards the other half of the hint.
// ToolSearch keyword search returns only its max_results best matches and
// defaults to 5, so a hint naming more tools than that silently loads a subset
// and the agent hits "not found" on whichever one was trimmed.
func TestRoutingBlocks_LiftToolSearchResultCap(t *testing.T) {
	for name, block := range map[string]string{
		"AnchoredRoutingBlock":    AnchoredRoutingBlock,
		"AnchoredMCPInstructions": AnchoredMCPInstructions,
		"AnchoredSubagentBlock":   AnchoredSubagentBlock,
	} {
		if !strings.Contains(block, "ToolSearch") {
			continue
		}
		if !strings.Contains(block, "max_results") {
			t.Errorf("%s asks for several tools without raising max_results (defaults to 5)", name)
		}
	}
}
