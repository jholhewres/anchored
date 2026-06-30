package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// anchored maintenance automates the periodic upkeep that otherwise only runs
// on demand or while an agent is connected to `anchored serve`:
//
//   - import   — pull fresh memories from Claude Code / OpenCode / Cursor
//   - dream    — consolidate (dedup, merge, flag contradictions)
//   - curation — reconcile quality/importance scores
//
// `anchored maintenance run` executes the three steps as isolated subprocesses
// (a failure in one step is logged but does not abort the others). `install`
// wires that command into a systemd --user timer so it runs every day without
// any agent connected — the same pattern as `anchored dashboard install`.

func runMaintenance(args []string) {
	if len(args) == 0 {
		printMaintenanceUsage()
		return
	}
	switch args[0] {
	case "run":
		runMaintenanceRun(args[1:])
	case "install":
		installMaintenanceService()
	case "uninstall":
		uninstallMaintenanceService()
	case "status":
		statusMaintenanceService()
	default:
		printMaintenanceUsage()
		os.Exit(2)
	}
}

func printMaintenanceUsage() {
	fmt.Fprintln(os.Stderr, "Usage: anchored maintenance <run|install|uninstall|status>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Periodic upkeep: import fresh memories, dream (consolidate),")
	fmt.Fprintln(os.Stderr, "and reconcile curation scores — either once or on a timer.")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  anchored maintenance run         run import + dream + curation now")
	fmt.Fprintln(os.Stderr, "  anchored maintenance install     install a systemd --user daily timer")
	fmt.Fprintln(os.Stderr, "  anchored maintenance uninstall   remove the timer")
	fmt.Fprintln(os.Stderr, "  anchored maintenance status      show timer status")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Flags for `run`:")
	fmt.Fprintln(os.Stderr, "  --config PATH            config file path")
	fmt.Fprintln(os.Stderr, "  --aggressiveness LEVEL   dream level: conservative|moderate|aggressive (default moderate)")
	fmt.Fprintln(os.Stderr, "  --max-deletions N        max soft-deletions per dream run (default 50)")
	fmt.Fprintln(os.Stderr, "  --skip-import            skip the import step")
	fmt.Fprintln(os.Stderr, "  --skip-dream             skip the dream step")
	fmt.Fprintln(os.Stderr, "  --skip-curation          skip the curation reconcile step")
}

// runMaintenanceRun executes the upkeep steps as isolated subprocesses.
// Subprocess isolation (rather than calling runImport/runDream directly) keeps
// a crash or os.Exit in one step from aborting the others, and mirrors what the
// systemd unit will do in production.
func runMaintenanceRun(args []string) {
	fs := newFlagSet("maintenance run")
	configPath := fs.String("config", "", "path to config file")
	aggressiveness := fs.String("aggressiveness", "moderate", "dream aggressiveness: conservative, moderate, aggressive")
	maxDeletions := fs.Int("max-deletions", 50, "maximum soft-deletions per dream run")
	skipImport := fs.Bool("skip-import", false, "skip the import step")
	skipDream := fs.Bool("skip-dream", false, "skip the dream step")
	skipCuration := fs.Bool("skip-curation", false, "skip the curation reconcile step")
	fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	exe, err := maintenanceExe()
	if err != nil {
		slog.Error("locate executable", "error", err)
		os.Exit(1)
	}

	start := time.Now()
	results := []struct {
		step string
		ok   bool
	}{}

	runStep := func(step string, skip bool, build func() *exec.Cmd) {
		if skip {
			logger.Info("maintenance: step skipped", "step", step)
			return
		}
		logger.Info("maintenance: step start", "step", step)
		t0 := time.Now()
		cmd := build()
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		dur := time.Since(t0).Round(time.Millisecond)
		if err != nil {
			logger.Error("maintenance: step failed", "step", step, "duration", dur, "error", err)
			results = append(results, struct {
				step string
				ok   bool
			}{step, false})
			return
		}
		logger.Info("maintenance: step done", "step", step, "duration", dur)
		results = append(results, struct {
			step string
			ok   bool
		}{step, true})
	}

	// 1. Import — pulls fresh memories from connected tools. Embeddings are
	// generated inline by the importer.
	runStep("import", *skipImport, func() *exec.Cmd {
		return maintenanceCmd(exe, *configPath, "import", "all")
	})

	// 2. Dream — analyze + apply consolidation. --dry-run=false forces apply
	// (the flag defaults to dry-run=true for interactive safety).
	runStep("dream", *skipDream, func() *exec.Cmd {
		cmd := maintenanceCmd(exe, *configPath, "dream",
			"--dry-run=false",
			"--aggressiveness", *aggressiveness,
			fmt.Sprintf("--max-deletions=%d", *maxDeletions),
		)
		return cmd
	})

	// 3. Curation — reconcile quality/importance metadata. --yes skips the
	// interactive confirmation prompt (unsupervised timer context).
	runStep("curation", *skipCuration, func() *exec.Cmd {
		return maintenanceCmd(exe, *configPath, "curation", "reconcile", "--yes")
	})

	failed := 0
	for _, r := range results {
		if !r.ok {
			failed++
		}
	}
	logger.Info("maintenance: run complete",
		"steps", len(results), "failed", failed, "duration", time.Since(start).Round(time.Millisecond))
	if failed > 0 {
		os.Exit(1)
	}
}

// maintenanceCmd builds a subprocess for one upkeep step, threading --config
// through only when set so the default config discovery still applies.
func maintenanceCmd(exe, configPath, sub string, extra ...string) *exec.Cmd {
	args := []string{sub}
	args = append(args, extra...)
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	return exec.Command(exe, args...)
}

// maintenanceExe resolves the anchored binary to invoke for sub-steps. Prefers
// the running executable (so the timer uses the exact version that installed
// it), falling back to PATH lookup.
func maintenanceExe() (string, error) {
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			return resolved, nil
		}
		return exe, nil
	}
	return exec.LookPath("anchored")
}

// --- systemd --user timer management (Linux only; best-effort) ---

const maintenanceUnitName = "anchored-maintenance"

func maintenanceUnitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// maintenanceUnit is the oneshot service the timer fires. It calls
// `anchored maintenance run`, which chains import + dream + curation.
func maintenanceUnit(exe string) string {
	return fmt.Sprintf(`[Unit]
Description=anchored periodic upkeep (import + dream + curation)

[Service]
Type=oneshot
ExecStart=%s maintenance run
`, exe)
}

// maintenanceTimer schedules the oneshot daily, with a small randomized delay
// so a fleet of machines doesn't stampede at exactly 04:00. Persistent=true
// catches up a missed run after suspend/shutdown.
func maintenanceTimer() string {
	return `# Persistent: run a missed activation after suspend/boot.
# RandomizedDelaySec spreads load if multiple hosts run this.
[Unit]
Description=anchored maintenance daily timer

[Timer]
OnCalendar=daily
Persistent=true
RandomizedDelaySec=15min

[Install]
WantedBy=timers.target
`
}

func installMaintenanceService() {
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "maintenance timer install is Linux-only (systemd --user).")
		fmt.Fprintln(os.Stderr, "Use cron to schedule `anchored maintenance run` on other platforms.")
		os.Exit(1)
	}
	exe, err := maintenanceExe()
	if err != nil {
		slog.Error("locate executable", "error", err)
		os.Exit(1)
	}

	dir, err := maintenanceUnitDir()
	if err != nil {
		slog.Error("resolve unit dir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("create unit dir", "error", err)
		os.Exit(1)
	}

	unitPath := filepath.Join(dir, maintenanceUnitName+".service")
	if err := os.WriteFile(unitPath, []byte(maintenanceUnit(exe)), 0o644); err != nil {
		slog.Error("write service unit", "error", err)
		os.Exit(1)
	}
	timerPath := filepath.Join(dir, maintenanceUnitName+".timer")
	if err := os.WriteFile(timerPath, []byte(maintenanceTimer()), 0o644); err != nil {
		slog.Error("write timer unit", "error", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "units written:\n  %s\n  %s\n", unitPath, timerPath)

	// Lingering lets the user service run after logout and start at boot;
	// harmless if already enabled or if loginctl is missing.
	if user := os.Getenv("USER"); user != "" {
		runCmd("loginctl", "enable-linger", user)
	}
	runCmd("systemctl", "--user", "daemon-reload")
	runCmd("systemctl", "--user", "enable", "--now", maintenanceUnitName+".timer")

	fmt.Fprintf(os.Stderr, "\nanchored maintenance timer enabled (runs daily).\n")
	fmt.Fprintf(os.Stderr, "  next run: systemctl --user list-timers %s.timer\n", maintenanceUnitName)
	fmt.Fprintf(os.Stderr, "  run now:  anchored maintenance run\n")
	fmt.Fprintf(os.Stderr, "  logs:     journalctl --user -u %s -f\n", maintenanceUnitName)
	fmt.Fprintf(os.Stderr, "  remove:   anchored maintenance uninstall\n")
}

func uninstallMaintenanceService() {
	runCmd("systemctl", "--user", "disable", "--now", maintenanceUnitName+".timer")
	if dir, err := maintenanceUnitDir(); err == nil {
		_ = os.Remove(filepath.Join(dir, maintenanceUnitName+".timer"))
		_ = os.Remove(filepath.Join(dir, maintenanceUnitName+".service"))
	}
	runCmd("systemctl", "--user", "daemon-reload")
	fmt.Fprintf(os.Stderr, "anchored maintenance timer removed.\n")
}

func statusMaintenanceService() {
	cmd := exec.Command("systemctl", "--user", "list-timers", maintenanceUnitName+".timer")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Fall back to the service status if the timer isn't installed.
		cmd2 := exec.Command("systemctl", "--user", "status", maintenanceUnitName+".service")
		cmd2.Stdout = os.Stdout
		cmd2.Stderr = os.Stderr
		_ = cmd2.Run()
	}
}
