package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jholhewres/anchored/pkg/hookroute"
)

func TestContextGate_SatisfyingToolMarksSession(t *testing.T) {
	dir := t.TempDir()
	dec, stage := contextGateDecision(dir, "sess-1", "anchored_context")
	if dec != nil {
		t.Fatalf("anchored_context should never be denied, got deny: %q", dec.Reason)
	}
	if stage != "satisfied" {
		t.Fatalf("want stage=satisfied, got %q", stage)
	}
	// A subsequent work tool must now pass.
	dec, stage = contextGateDecision(dir, "sess-1", "Bash")
	if dec != nil {
		t.Fatalf("work tool after satisfy should pass, got deny (stage=%q)", stage)
	}
	if stage != "already_satisfied" {
		t.Fatalf("want stage=already_satisfied, got %q", stage)
	}
}

func TestContextGate_DeniesFirstWorkTool(t *testing.T) {
	dir := t.TempDir()
	dec, stage := contextGateDecision(dir, "sess-2", "Bash")
	if dec == nil {
		t.Fatalf("first work tool should be denied, got passthrough (stage=%q)", stage)
	}
	if dec.Action != hookroute.ActionDeny {
		t.Fatalf("want ActionDeny, got %q", dec.Action)
	}
	if stage != "denied" {
		t.Fatalf("want stage=denied, got %q", stage)
	}
	// Marker should hold the deny counter (1).
	data, err := os.ReadFile(filepath.Join(dir, "ctxgate", sanitizeSessionID("sess-2")))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if string(data) != "1" {
		t.Fatalf("want deny counter 1, got %q", data)
	}
}

func TestContextGate_RelentsAfterBudget(t *testing.T) {
	dir := t.TempDir()
	// Exhaust the deny budget.
	for i := 0; i < ctxGateMaxDenies; i++ {
		dec, _ := contextGateDecision(dir, "sess-3", "Bash")
		if dec == nil {
			t.Fatalf("deny %d should still block", i+1)
		}
	}
	// Next call must relent (pass through) rather than block forever.
	dec, stage := contextGateDecision(dir, "sess-3", "Bash")
	if dec != nil {
		t.Fatalf("gate must relent after %d denies, still blocking (stage=%q)", ctxGateMaxDenies, stage)
	}
	if stage != "relented" {
		t.Fatalf("want stage=relented, got %q", stage)
	}
	// And stay relented.
	if _, stage := contextGateDecision(dir, "sess-3", "Bash"); stage != "already_satisfied" {
		t.Fatalf("after relent want already_satisfied, got %q", stage)
	}
}

func TestContextGate_NeverGatesAnchoredTools(t *testing.T) {
	dir := t.TempDir()
	// An anchored tool that is NOT a satisfying one (e.g. a save) must pass
	// without ever being gated, even on a fresh session — otherwise the agent
	// could be deadlocked.
	dec, stage := contextGateDecision(dir, "sess-4", "anchored_save")
	if dec != nil {
		t.Fatalf("anchored tools must never be gated, got deny (stage=%q)", stage)
	}
	if stage != "exempt_anchored" {
		t.Fatalf("want stage=exempt_anchored, got %q", stage)
	}
}

func TestContextGate_FailOpenWithoutSession(t *testing.T) {
	dir := t.TempDir()
	if dec, stage := contextGateDecision(dir, "", "Bash"); dec != nil || stage != "skip_no_session" {
		t.Fatalf("empty session id must fail open, got dec=%v stage=%q", dec, stage)
	}
	if dec, stage := contextGateDecision("", "sess-5", "Bash"); dec != nil || stage != "skip_no_session" {
		t.Fatalf("empty storage dir must fail open, got dec=%v stage=%q", dec, stage)
	}
}

func TestContextGate_SearchAlsoSatisfies(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"anchored_search", "anchored_ctx_search", "anchored_kg_query"} {
		sess := "sess-" + tool
		if _, stage := contextGateDecision(dir, sess, tool); stage != "satisfied" {
			t.Fatalf("%s should satisfy the gate, got %q", tool, stage)
		}
	}
}

// TestMCPLeafName_PrefixAgnostic pins where prefix-agnosticism actually lives.
// The gate itself never sees a wire name — it is handed a leaf — so this
// derivation is the single point where a drifting registration prefix could
// break every anchored-tool decision at once. It used to be copy-pasted into
// three hooks.
func TestMCPLeafName_PrefixAgnostic(t *testing.T) {
	for _, tc := range []struct{ wire, want string }{
		{"mcp__anchored__anchored_context", "anchored_context"},
		{"mcp__plugin_anchored_anchored__anchored_context", "anchored_context"},
		{"mcp__some_future_prefix__anchored_save", "anchored_save"},
		{"mcp__anchored__anchored_execute", "anchored_execute"},
		{"Bash", "Bash"},
		{"", ""},
	} {
		if got := mcpLeafName(tc.wire); got != tc.want {
			t.Errorf("mcpLeafName(%q) = %q, want %q", tc.wire, got, tc.want)
		}
	}
}

// TestContextGate_SatisfiedViaDerivedLeaf runs the real derivation into the
// gate, so a regression in either half fails here.
func TestContextGate_SatisfiedViaDerivedLeaf(t *testing.T) {
	for _, wire := range []string{
		"mcp__anchored__anchored_context",
		"mcp__plugin_anchored_anchored__anchored_context",
		"mcp__some_future_prefix__anchored_search",
	} {
		dir := t.TempDir()
		if _, stage := contextGateDecision(dir, "sess-6", mcpLeafName(wire)); stage != "satisfied" {
			t.Errorf("%s should satisfy the gate, got %q", wire, stage)
		}
		if dec, stage := contextGateDecision(dir, "sess-6", "Bash"); dec != nil || stage != "already_satisfied" {
			t.Errorf("%s: session should stay satisfied, got dec=%v stage=%q", wire, dec, stage)
		}
	}
}

func TestContextGate_DenyHintHasNoHardcodedFQN(t *testing.T) {
	dir := t.TempDir()
	dec, _ := contextGateDecision(dir, "sess-7", "Bash")
	if dec == nil {
		t.Fatal("first work tool should be denied")
	}
	if strings.Contains(dec.Reason, "select:mcp__") {
		t.Fatalf("deny hint must not hardcode an MCP FQN (registration prefixes vary by harness): %q", dec.Reason)
	}
	if !strings.Contains(dec.Reason, "ToolSearch") {
		t.Fatalf("deny hint should still point at ToolSearch: %q", dec.Reason)
	}
}

func TestSanitizeSessionID(t *testing.T) {
	if got := sanitizeSessionID(""); got != "_.gate" {
		t.Fatalf("empty id: want _.gate, got %q", got)
	}
	if got := sanitizeSessionID("a/b:c d"); got != "a_b_c_d.gate" {
		t.Fatalf("unsafe runes: want a_b_c_d.gate, got %q", got)
	}
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if got := sanitizeSessionID(long); len(got) != 128+len(".gate") {
		t.Fatalf("overlong id not capped: len=%d", len(got))
	}
}

// ─── Recall-side satisfaction ───────────────────────────────────────────────

// TestGateFromRecall_RequiresRichBlock pins the conservative half of the rule:
// injected hits alone must NOT credit the gate. The recall pass is keyword-only
// and never carries identity (L0), so without SessionStart's rich block the
// session genuinely has not seen what anchored_context would have shown it.
func TestGateFromRecall_RequiresRichBlock(t *testing.T) {
	dir := t.TempDir()

	if satisfyGateFromRecall(dir, "sess-recall-1") {
		t.Fatal("credited the gate on hits alone, without the SessionStart rich block")
	}

	// The gate must still deny after a hits-only recall.
	dec, stage := contextGateDecision(dir, "sess-recall-1", "Bash")
	if dec == nil {
		t.Fatalf("expected deny after uncredited recall, got passthrough (stage=%s)", stage)
	}
}

// TestGateFromRecall_CreditsWhenBothSignalsLand is the fix itself: when
// SessionStart delivered the rich block AND the recall injected memories, the
// model already has what the gate would force it to fetch, so the first work
// tool must pass.
func TestGateFromRecall_CreditsWhenBothSignalsLand(t *testing.T) {
	dir := t.TempDir()
	const sess = "sess-recall-2"

	markSessionStartRichBlock(dir, sess)
	if !satisfyGateFromRecall(dir, sess) {
		t.Fatal("rich block + injected hits did not credit the gate")
	}

	dec, stage := contextGateDecision(dir, sess, "Bash")
	if dec != nil {
		t.Fatalf("gate denied a session whose memory was already delivered (stage=%s)", stage)
	}
	if stage != "already_satisfied" {
		t.Fatalf("stage = %q, want already_satisfied", stage)
	}
}

// TestGateFromRecall_CreditsOnceThenShortCircuits pins the hot-path guarantee:
// this runs on every user prompt inside the injection budget, so only the first
// call may write. A later call must report that it did nothing and leave the
// existing credit exactly as it found it.
//
// (The "recall found nothing" edge is the caller's: creditGateFromRecall
// measures the rendered preview, which is covered by
// TestCreditGateFromRecall_OnlyWhenSomethingWasRendered.)
func TestGateFromRecall_CreditsOnceThenShortCircuits(t *testing.T) {
	dir := t.TempDir()
	const sess = "sess-recall-3"
	gateDir := filepath.Join(dir, "ctxgate")
	okMarker := filepath.Join(gateDir, sanitizeSessionID(sess)) + ctxGateOKMarkerSuffix

	markSessionStartRichBlock(dir, sess)
	if !satisfyGateFromRecall(dir, sess) {
		t.Fatal("first credit did not land")
	}
	info, err := os.Stat(okMarker)
	if err != nil {
		t.Fatalf("stat sentinel: %v", err)
	}

	if satisfyGateFromRecall(dir, sess) {
		t.Error("a second call reported a fresh credit for an already-credited session")
	}
	again, err := os.Stat(okMarker)
	if err != nil {
		t.Fatalf("stat sentinel after second call: %v", err)
	}
	if !again.ModTime().Equal(info.ModTime()) {
		t.Error("the second call rewrote the sentinel instead of short-circuiting")
	}

	if dec, stage := contextGateDecision(dir, sess, "Bash"); dec != nil {
		t.Fatalf("gate denied a credited session (stage=%s)", stage)
	}
}

// TestGateFromRecall_FailsOpenOnMissingInputs keeps the whole path inside the
// gate's fail-open contract: no session id and no storage dir mean no credit
// and no panic, never a block.
func TestGateFromRecall_FailsOpenOnMissingInputs(t *testing.T) {
	dir := t.TempDir()

	markSessionStartRichBlock("", "sess-recall-4")
	markSessionStartRichBlock(dir, "")

	if satisfyGateFromRecall("", "sess-recall-4") {
		t.Error("credited with an empty storage dir")
	}
	if satisfyGateFromRecall(dir, "") {
		t.Error("credited with an empty session id")
	}
}

// TestGateFromRecall_RichMarkerSweptWithGateMarkers ensures the companion
// marker does not turn ctxgate/ into an unbounded directory: it shares the
// suffix-agnostic TTL sweep with the gate markers themselves.
func TestGateFromRecall_RichMarkerSweptWithGateMarkers(t *testing.T) {
	dir := t.TempDir()
	const sess = "sess-recall-5"

	markSessionStartRichBlock(dir, sess)
	gateDir := filepath.Join(dir, "ctxgate")
	rich := richMarkerPath(gateDir, sess)

	stale := time.Now().Add(-2 * ctxGateMarkerTTL)
	if err := os.Chtimes(rich, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	sweepStaleGateMarkers(gateDir)

	if _, err := os.Stat(rich); !os.IsNotExist(err) {
		t.Fatalf("stale rich marker survived the sweep (err=%v)", err)
	}
}

// TestContextGate_ExemptsNonSatisfyingAnchoredTool reaches isAnchoredTool for
// real. The pre-existing prefix test never did: it passed anchored_context,
// which short-circuits on contextGateSatisfyingTools before isAnchoredTool is
// ever called. anchored_save is an anchored tool that does NOT satisfy the
// gate, so it is the only way into the exemption branch — and gating it would
// deadlock a session whose only way out is calling an anchored tool.
func TestContextGate_ExemptsNonSatisfyingAnchoredTool(t *testing.T) {
	dir := t.TempDir()

	for _, full := range []string{
		"mcp__anchored__anchored_save",
		"mcp__plugin_anchored_anchored__anchored_save",
		"mcp__some_future_prefix__anchored_save",
	} {
		dec, stage := contextGateDecision(dir, "sess-exempt", "anchored_save")
		if dec != nil || stage != "exempt_anchored" {
			t.Errorf("%s: dec=%v stage=%q, want passthrough/exempt_anchored", full, dec, stage)
		}
	}

	// Exemption must not be mistaken for satisfaction: a work tool afterwards
	// is still denied.
	if dec, _ := contextGateDecision(dir, "sess-exempt", "Bash"); dec == nil {
		t.Error("exempting an anchored tool must not satisfy the gate")
	}
}

// TestContextGate_SatisfactionSurvivesConcurrentDenies pins the monotonicity
// that the separate sentinel buys. Before it, satisfaction lived inside the
// counter file, so a deny write that raced with a credit could overwrite "ok"
// with a counter and re-arm a gate the agent had already satisfied — breaking
// the module's "can never be permanently wedged" contract.
func TestContextGate_SatisfactionSurvivesConcurrentDenies(t *testing.T) {
	dir := t.TempDir()
	const sess = "sess-race"
	gateDir := filepath.Join(dir, "ctxgate")
	marker := filepath.Join(gateDir, sanitizeSessionID(sess))

	// Arm the gate, then credit it, then simulate a deny write that was still
	// in flight when the credit landed.
	if dec, _ := contextGateDecision(dir, sess, "Bash"); dec == nil {
		t.Fatal("first work tool should be denied")
	}
	markGateSatisfied(gateDir, marker)
	writeGateMarker(gateDir, marker, "2") // the stale, racing counter write

	if dec, stage := contextGateDecision(dir, sess, "Bash"); dec != nil {
		t.Fatalf("a stale counter write revoked the credit (stage=%q)", stage)
	}
}

// TestContextGate_LegacyOKContentStillSatisfies keeps an upgrade from re-arming
// live sessions: builds before the sentinel wrote "ok" into the counter file
// itself, and those markers are on disk.
func TestContextGate_LegacyOKContentStillSatisfies(t *testing.T) {
	dir := t.TempDir()
	const sess = "sess-legacy"
	gateDir := filepath.Join(dir, "ctxgate")
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gateDir, sanitizeSessionID(sess)), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	if dec, stage := contextGateDecision(dir, sess, "Bash"); dec != nil {
		t.Fatalf("legacy satisfied marker was ignored (stage=%q)", stage)
	}
}

// TestWriteGateMarker_NeverObservedEmpty guards the atomic write. A plain
// os.WriteFile opens O_TRUNC, so a concurrent reader can observe a zero-length
// marker and read the deny counter as 0 — which makes the relent bound
// unenforceable. With temp-file + rename a reader sees old content or new,
// never nothing.
func TestWriteGateMarker_NeverObservedEmpty(t *testing.T) {
	dir := t.TempDir()
	gateDir := filepath.Join(dir, "ctxgate")
	marker := filepath.Join(gateDir, "probe.gate")
	writeGateMarker(gateDir, marker, "1")

	done := make(chan struct{})
	var empties int64
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			writeGateMarker(gateDir, marker, strconv.Itoa(i%9+1))
		}
	}()
	for {
		select {
		case <-done:
			if empties > 0 {
				t.Fatalf("observed a zero-length marker %d times during concurrent writes", empties)
			}
			return
		default:
			if data, err := os.ReadFile(marker); err == nil && len(data) == 0 {
				empties++
			}
		}
	}
}
