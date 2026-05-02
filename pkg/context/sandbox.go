//go:build !windows

package ctx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	defaultTimeout    = 30 * time.Second
	defaultMaxOutput  = 1 << 20
	tempDirPrefix     = "anchored-exec-"
)

type Sandbox struct {
	timeout        time.Duration
	maxOutputBytes int64
	workDir        string
}

func NewSandbox(timeout time.Duration, maxOutputBytes int64, workDir string) *Sandbox {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutput
	}
	return &Sandbox{
		timeout:        timeout,
		maxOutputBytes: maxOutputBytes,
		workDir:        workDir,
	}
}

// limitedWriter silently discards after cap. Always returns (len(p), nil) to
// prevent SIGPIPE / pipe deadlock when the subprocess writes past the limit.
type limitedWriter struct {
	buf       bytes.Buffer
	capBytes  int64
	truncated bool
}

func newLimitedWriter(capBytes int64) *limitedWriter {
	return &limitedWriter{capBytes: capBytes}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.truncated {
		return len(p), nil
	}
	remaining := w.capBytes - int64(w.buf.Len())
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		w.buf.Write(p[:remaining])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (s *Sandbox) Execute(parent context.Context, language string, code string) (*ExecuteResult, error) {
	tmpDir, err := os.MkdirTemp("", tempDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	runtime, args, fileName, err := s.buildCommand(language, tmpDir)
	if err != nil {
		return nil, err
	}

	codePath := filepath.Join(tmpDir, fileName)
	if err := os.WriteFile(codePath, []byte(code), 0644); err != nil {
		return nil, fmt.Errorf("write temp file: %w", err)
	}

	cmd := exec.Command(runtime, args...)
	cmd.Dir = s.workDir
	if cmd.Dir == "" {
		cmd.Dir = tmpDir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutWriter := newLimitedWriter(s.maxOutputBytes)
	var stderrBuf bytes.Buffer
	cmd.Stdout = stdoutWriter
	cmd.Stderr = &stderrBuf

	ctx, cancel := context.WithTimeout(parent, s.timeout)
	defer cancel()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command: %w", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}

	duration := time.Since(start)

	result := &ExecuteResult{
		Stdout:    stdoutWriter.buf.String(),
		Stderr:    stderrBuf.String(),
		Duration:  duration,
		TimedOut:  ctx.Err() == context.DeadlineExceeded,
		Truncated: stdoutWriter.truncated,
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = 1
		}
	}

	return result, nil
}

func (s *Sandbox) buildCommand(language string, tmpDir string) (string, []string, string, error) {
	switch language {
	case "javascript":
		runtime := findRuntime([]string{"bun", "node"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no JavaScript runtime found (tried: bun, node)")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.js")}, "script.js", nil

	case "typescript":
		runtime := findRuntime([]string{"bun", "npx"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no TypeScript runtime found (tried: bun, npx tsx)")
		}
		if filepath.Base(runtime) == "bun" {
			return runtime, []string{filepath.Join(tmpDir, "script.ts")}, "script.ts", nil
		}
		return runtime, []string{"tsx", filepath.Join(tmpDir, "script.ts")}, "script.ts", nil

	case "python":
		runtime := findRuntime([]string{"python3", "python"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no Python runtime found (tried: python3, python)")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.py")}, "script.py", nil

	case "shell":
		return "bash", []string{filepath.Join(tmpDir, "script.sh")}, "script.sh", nil

	case "go":
		return "go", []string{"run", filepath.Join(tmpDir, "script.go")}, "script.go", nil

	case "ruby":
		runtime := findRuntime([]string{"ruby"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no Ruby runtime found")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.rb")}, "script.rb", nil

	case "rust":
		return "bash", []string{"-c", fmt.Sprintf("rustc %s -o %s && %s", filepath.Join(tmpDir, "script.rs"), filepath.Join(tmpDir, "script_out"), filepath.Join(tmpDir, "script_out"))}, "script.rs", nil

	case "php":
		runtime := findRuntime([]string{"php"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no PHP runtime found")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.php")}, "script.php", nil

	case "perl":
		runtime := findRuntime([]string{"perl"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no Perl runtime found")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.pl")}, "script.pl", nil

	case "r":
		runtime := findRuntime([]string{"Rscript"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no R runtime found")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.R")}, "script.R", nil

	case "elixir":
		runtime := findRuntime([]string{"elixir"})
		if runtime == "" {
			return "", nil, "", fmt.Errorf("no Elixir runtime found")
		}
		return runtime, []string{filepath.Join(tmpDir, "script.exs")}, "script.exs", nil

	default:
		return "", nil, "", fmt.Errorf("unsupported language: %s", language)
	}
}

func findRuntime(candidates []string) string {
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
