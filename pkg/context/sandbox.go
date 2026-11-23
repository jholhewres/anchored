//go:build !windows

package ctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultMaxOutput = 1 << 20
	tempDirPrefix    = "anchored-exec-"
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

// sandboxWaitDelay bounds the wait for pipes an escaped descendant keeps open.
const sandboxWaitDelay = time.Second

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
	cmd.Env = sanitizedEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Both streams are capped: an execution that floods stderr must not hold
	// it all in memory.
	stdoutWriter := newLimitedWriter(s.maxOutputBytes)
	stderrWriter := newLimitedWriter(s.maxOutputBytes)
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	// A descendant that escapes the process group (setsid) can keep the
	// pipes open after the command exits or is killed; Wait gives up on
	// them this long after, so the execution ends on time.
	cmd.WaitDelay = sandboxWaitDelay

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
		Stderr:    stderrWriter.buf.String(),
		Duration:  duration,
		TimedOut:  ctx.Err() == context.DeadlineExceeded,
		Truncated: stdoutWriter.truncated,
	}

	if errors.Is(waitErr, exec.ErrWaitDelay) {
		waitErr = nil // the command exited; only an escaped descendant held its pipes
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

// envDenylist names environment variables that can hijack interpreters or
// pollute sandbox stdout. They are stripped before launching the subprocess.
var envDenylist = map[string]struct{}{
	"BASH_ENV":                {},
	"ENV":                     {},
	"PROMPT_COMMAND":          {},
	"PS4":                     {},
	"NODE_OPTIONS":            {},
	"NODE_PATH":               {},
	"NODE_DISABLE_COLORS":     {},
	"PYTHONSTARTUP":           {},
	"PYTHONDONTWRITEBYTECODE": {},
	"PYTHONOPTIMIZE":          {},
	"PYTHONPATH":              {},
	"RUBYOPT":                 {},
	"RUBYLIB":                 {},
	"RUBYLIB_PREFIX":          {},
	"GEM_HOME":                {},
	"GEM_PATH":                {},
	"PERL5OPT":                {},
	"PERL5LIB":                {},
	"PHPRC":                   {},
	"R_PROFILE":               {},
	"R_PROFILE_USER":          {},
	"R_ENVIRON":               {},
	"R_ENVIRON_USER":          {},
	"GOFLAGS":                 {},
	"GOENV":                   {},
	"RUSTFLAGS":               {},
	"RUSTC_WRAPPER":           {},
	"CARGO_HOME":              {},
	"LD_PRELOAD":              {},
	"LD_AUDIT":                {},
	"LD_LIBRARY_PATH":         {},
	"DYLD_INSERT_LIBRARIES":   {},
	"DYLD_LIBRARY_PATH":       {},
	"DYLD_FRAMEWORK_PATH":     {},
	"GIT_SSH_COMMAND":         {},
	"GIT_EXTERNAL_DIFF":       {},
	"GIT_PAGER":               {},
	"PAGER":                   {},
	"EDITOR":                  {},
	"VISUAL":                  {},
	"MANPAGER":                {},
	"LESS":                    {},
	"DEBUGINFOD_URLS":         {},
	"PIP_INDEX_URL":           {},
	"PIP_EXTRA_INDEX_URL":     {},
}

// sanitizedEnv returns os.Environ() with denylisted vars removed and forced
// sandbox-friendly settings applied (unbuffered, no color, minimal pager).
func sanitizedEnv() []string {
	out := make([]string, 0, len(os.Environ())+4)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := kv[:eq]
		if _, blocked := envDenylist[key]; blocked {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"PYTHONUNBUFFERED=1",
		"NO_COLOR=1",
		"FORCE_COLOR=0",
		"TERM=dumb",
	)
	return out
}

// FilePrelude returns the language-specific snippet that exposes the file's
// content to user code via well-known variables. The user code is then
// concatenated after this prelude and executed.
//
//	FILE_PATH      — absolute path to the file (string).
//	FILE_CONTENT   — the file's contents read as text (string).
//
// For shell, FILE_PATH is exported and FILE_CONTENT is loaded into the variable.
func FilePrelude(language, path string) string {
	// Escape for the target language. The path may contain spaces or quotes;
	// we use language-native quoting to keep the value intact.
	switch language {
	case "javascript", "typescript":
		return fmt.Sprintf(
			"const FILE_PATH = %q;\nconst FILE_CONTENT = require('fs').readFileSync(FILE_PATH, 'utf8');\n",
			path,
		)
	case "python":
		return fmt.Sprintf(
			"FILE_PATH = %q\nwith open(FILE_PATH, 'r', encoding='utf-8') as _f: FILE_CONTENT = _f.read()\n",
			path,
		)
	case "shell":
		return fmt.Sprintf(
			"export FILE_PATH=%s\nFILE_CONTENT=\"$(cat \"$FILE_PATH\")\"\nexport FILE_CONTENT\n",
			shellQuote(path),
		)
	case "ruby":
		return fmt.Sprintf(
			"FILE_PATH = %q\nFILE_CONTENT = File.read(FILE_PATH)\n",
			path,
		)
	case "go":
		return fmt.Sprintf(
			"// auto-injected; access via os.Getenv(\"FILE_PATH\")\nvar _filePath = %q\n_ = _filePath\n",
			path,
		)
	case "rust":
		return fmt.Sprintf(
			"const FILE_PATH: &str = %q;\nlet _file_content = std::fs::read_to_string(FILE_PATH).unwrap_or_default();\n",
			path,
		)
	case "php":
		return fmt.Sprintf(
			"<?php\n$FILE_PATH = %q;\n$FILE_CONTENT = file_get_contents($FILE_PATH);\n?>\n",
			path,
		)
	case "perl":
		return fmt.Sprintf(
			"my $FILE_PATH = %q;\nmy $FILE_CONTENT = do { local(@ARGV, $/) = ($FILE_PATH); <> };\n",
			path,
		)
	case "r":
		return fmt.Sprintf(
			"FILE_PATH <- %q\nFILE_CONTENT <- paste(readLines(FILE_PATH, warn=FALSE), collapse=\"\\n\")\n",
			path,
		)
	case "elixir":
		return fmt.Sprintf(
			"file_path = %q\nfile_content = File.read!(file_path)\n",
			path,
		)
	default:
		// Languages we cannot prelude — caller can still resolve via os.Getenv.
		return ""
	}
}

// shellQuote single-quotes a string for safe inclusion in a bash command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
