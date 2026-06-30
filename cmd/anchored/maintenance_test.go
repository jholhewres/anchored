package main

import (
	"strings"
	"testing"
)

func TestMaintenanceUnit_ContainsExecStart(t *testing.T) {
	got := maintenanceUnit("/usr/local/bin/anchored")
	if !strings.Contains(got, "Type=oneshot") {
		t.Errorf("unit must be oneshot, got:\n%s", got)
	}
	if !strings.Contains(got, "ExecStart=/usr/local/bin/anchored maintenance run") {
		t.Errorf("ExecStart must run `maintenance run`, got:\n%s", got)
	}
	if !strings.Contains(got, "Description=anchored periodic upkeep") {
		t.Errorf("missing description, got:\n%s", got)
	}
}

func TestMaintenanceTimer_DailyPersistentJittered(t *testing.T) {
	got := maintenanceTimer()
	for _, want := range []string{"OnCalendar=daily", "Persistent=true", "RandomizedDelaySec=15min", "WantedBy=timers.target"} {
		if !strings.Contains(got, want) {
			t.Errorf("timer missing %q, got:\n%s", want, got)
		}
	}
}

func TestMaintenanceCmd_ThreadsConfig(t *testing.T) {
	// With configPath set, --config is appended after the step's own flags.
	cmd := maintenanceCmd("/x/anchored", "/etc/anchored.yaml", "dream", "--dry-run=false")
	got := strings.Join(cmd.Args, " ")
	want := "/x/anchored dream --dry-run=false --config /etc/anchored.yaml"
	if got != want {
		t.Errorf("cmd with config: got %q, want %q", got, want)
	}

	// Without configPath, no --config flag is added (default discovery applies).
	cmd2 := maintenanceCmd("/x/anchored", "", "import", "all")
	got2 := strings.Join(cmd2.Args, " ")
	want2 := "/x/anchored import all"
	if got2 != want2 {
		t.Errorf("cmd without config: got %q, want %q", got2, want2)
	}
}

// TestRunMaintenanceRun_AllSkipped exercises the orchestration loop without
// touching the DB or ONNX: every step is skipped, so no subprocess is spawned
// and the run completes with zero steps. Validates that the dispatcher, flag
// parsing, and completion logging hold together end-to-end. The success path
// does not call os.Exit, so this is safe to invoke directly.
func TestRunMaintenanceRun_AllSkipped(t *testing.T) {
	// Discard the structured logs so the test output stays clean.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("runMaintenanceRun panicked: %v", r)
		}
	}()
	runMaintenanceRun([]string{
		"--skip-import", "--skip-dream", "--skip-curation",
	})
}
