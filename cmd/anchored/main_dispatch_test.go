package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// MCP clients register a bare `anchored`, and some pass flags first
// (`anchored --config x`): both are the server. Any other word that is not a
// subcommand used to boot a full MCP server too (ONNX, workers, a writer on
// the database) instead of saying the command does not exist.
func TestUnrecognisedArgStartsServerOnlyForFlags(t *testing.T) {
	cases := map[string]bool{
		"--config": true,
		"-config":  true,
		"version2": false,
		"xyz":      false,
		"":         false,
	}
	for arg, want := range cases {
		if got := unrecognisedArgIsServe(arg); got != want {
			t.Errorf("unrecognisedArgIsServe(%q) = %v, want %v", arg, got, want)
		}
	}
}

// `anchored serve --stdio` is the documented way to run the server locally;
// stdio is the only transport, so the flag is accepted and ignored.
func TestServeConfigPath_AcceptsStdio(t *testing.T) {
	if got := serveConfigPath([]string{"--stdio", "--config", "/x/config.yaml"}); got != "/x/config.yaml" {
		t.Errorf("serveConfigPath = %q, want /x/config.yaml", got)
	}
	if got := serveConfigPath([]string{"--stdio"}); got != "" {
		t.Errorf("serveConfigPath = %q, want empty", got)
	}
}

// TestMainHelper is not a test: TestMain_UnknownCommandExitsWithUsage re-runs
// the test binary into it so main() executes with real os.Args and os.Exit.
func TestMainHelper(t *testing.T) {
	if os.Getenv("ANCHORED_TEST_MAIN_ARGS") == "" {
		t.Skip("helper process only")
	}
	os.Args = append([]string{"anchored"}, strings.Fields(os.Getenv("ANCHORED_TEST_MAIN_ARGS"))...)
	main()
	os.Exit(0)
}

func TestMain_UnknownCommandExitsWithUsage(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelper$")
	cmd.Env = append(os.Environ(), "ANCHORED_TEST_MAIN_ARGS=xyz", "ANCHORED_NO_AUTOUPDATE=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("anchored xyz must exit 2, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), `unknown command "xyz"`) {
		t.Errorf("anchored xyz must say the command is unknown:\n%s", out)
	}
}

func TestMain_VersionPrintsWithoutServing(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainHelper$")
	cmd.Env = append(os.Environ(), "ANCHORED_TEST_MAIN_ARGS=version", "ANCHORED_NO_AUTOUPDATE=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "anchored ") {
		t.Fatalf("anchored version: %v\n%s", err, out)
	}
}
