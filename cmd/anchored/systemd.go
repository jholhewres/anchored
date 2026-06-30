package main

import (
	"fmt"
	"os"
	"os/exec"
)

// runCmd runs a command, streaming its output to stderr. Failure is non-fatal:
// the systemd --user service management (dashboard install / maintenance
// install) must degrade gracefully across distros and environments where
// systemctl or loginctl may be missing or restricted. Shared by the dashboard
// and maintenance installers — keep it here so neither owns the other's helper.
func runCmd(name string, args ...string) bool {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  (warning) %s: %v\n", name, err)
		return false
	}
	return true
}
