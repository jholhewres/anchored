package main

import (
	"os/exec"
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

	// The backfill step caps memories embedded per run so the daily timer
	// drains a large backlog in slices instead of one multi-hour pass.
	cmd3 := maintenanceCmd("/x/anchored", "", "backfill", "--max", "2000")
	got3 := strings.Join(cmd3.Args, " ")
	want3 := "/x/anchored backfill --max 2000"
	if got3 != want3 {
		t.Errorf("cmd for backfill: got %q, want %q", got3, want3)
	}
}

// TestRunMaintenanceRun_AllSkipped exercises the orchestration loop without
// touching the DB or ONNX: every step is skipped, so the run completes with
// zero steps. Validates that the dispatcher, flag parsing, and completion
// logging hold together end-to-end. The success path does not call os.Exit, so
// this is safe to invoke directly.
func TestRunMaintenanceRun_AllSkipped(t *testing.T) {
	spawned := interceptMaintenanceSteps(t)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("runMaintenanceRun panicked: %v", r)
		}
	}()
	runMaintenanceRun([]string{
		"--skip-import", "--skip-backfill", "--skip-dream", "--skip-curation",
		"--skip-compact",
	})
	if len(*spawned) != 0 {
		t.Fatalf("all steps skipped but %v ran", *spawned)
	}
}

// Every step must have a --skip flag. A step without one turns this test into
// unbounded recursion under `go test`, because the binary a step runs is the
// test suite itself; the seam below keeps the failure a failing assertion.
func TestRunMaintenanceRun_EveryStepIsSkippable(t *testing.T) {
	spawned := interceptMaintenanceSteps(t)
	runMaintenanceRun(allMaintenanceSkipFlags)
	if len(*spawned) != 0 {
		t.Fatalf("steps ran despite every skip flag: %v — a step is missing a --skip flag", *spawned)
	}

	// And with nothing skipped, every known step is attempted exactly once.
	ran := interceptMaintenanceSteps(t)
	runMaintenanceRun(nil)
	if len(*ran) != len(allMaintenanceSkipFlags) {
		t.Fatalf("steps run = %v, want one per skip flag (%d)", *ran, len(allMaintenanceSkipFlags))
	}
}

var allMaintenanceSkipFlags = []string{
	"--skip-import", "--skip-backfill", "--skip-dream", "--skip-curation",
	"--skip-compact",
}

// interceptMaintenanceSteps swaps the subprocess seam for a recorder and
// restores it when the test ends.
func interceptMaintenanceSteps(t *testing.T) *[]string {
	t.Helper()
	original, originalExe := runMaintenanceStep, resolveMaintenanceExe
	var spawned []string
	runMaintenanceStep = func(cmd *exec.Cmd) error {
		spawned = append(spawned, strings.Join(cmd.Args[1:], " "))
		return nil
	}
	// Under go test the real lookup falls back to PATH, where CI has no
	// anchored binary; the steps never run, so any path will do.
	resolveMaintenanceExe = func() (string, error) { return "/nonexistent/anchored", nil }
	t.Cleanup(func() { runMaintenanceStep, resolveMaintenanceExe = original, originalExe })
	return &spawned
}

func TestMaintenanceExeRefusesTestBinary(t *testing.T) {
	if !isTestBinary("/tmp/go-build123/b001/anchored.test") {
		t.Fatal("a .test binary must be recognised")
	}
	if !isTestBinary("/usr/local/bin/anchored") {
		// Running under `go test`, os.Args carries -test.* flags, so even a
		// clean path is refused. That is the behaviour that matters here.
		t.Fatal("running under go test must be recognised")
	}
	if isTestBinaryPath("/usr/local/bin/anchored") {
		t.Fatal("a release binary path must not look like a test binary")
	}
}
