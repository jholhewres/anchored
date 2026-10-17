package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
)

// runCLI runs `anchored <args>` in a subprocess with HOME and the working
// directory set to throwaway folders, and returns its combined output and
// exit code.
func runCLI(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelper$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"ANCHORED_TEST_MAIN_ARGS="+strings.Join(args, " "),
		"ANCHORED_NO_AUTOUPDATE=1",
		"HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("run anchored %v: %v", args, err)
		return "", -1
	}
}

func newRemoteTestEnv(t *testing.T, remotes map[string]config.RemoteEntry) string {
	t.Helper()
	return newTestEnvWith(t, func(cfg *config.Config) { cfg.Remotes = remotes })
}

func TestRemoteStatus_WarnsAboutDuplicateDefaults(t *testing.T) {
	cfgPath := newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"personal": {ServerURL: "http://127.0.0.1:1", APIKey: "<REDACTED>", Default: true},
		"company":  {ServerURL: "http://127.0.0.1:2", APIKey: "<REDACTED>", Default: true},
	})

	out, code := runCLI(t, t.TempDir(), "remote", "status", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote status exited %d\n%s", code, out)
	}
	if !strings.Contains(out, `2 remotes are marked default (company, personal); routing uses "company"`) {
		t.Errorf("remote status must warn about duplicate defaults:\n%s", out)
	}

	cfgPath = newRemoteTestEnv(t, map[string]config.RemoteEntry{
		"personal": {ServerURL: "http://127.0.0.1:1", APIKey: "<REDACTED>", Default: true},
		"company":  {ServerURL: "http://127.0.0.1:2", APIKey: "<REDACTED>"},
	})
	out, code = runCLI(t, t.TempDir(), "remote", "status", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("remote status exited %d\n%s", code, out)
	}
	if strings.Contains(out, "marked default") {
		t.Errorf("a single default must not warn:\n%s", out)
	}
}
