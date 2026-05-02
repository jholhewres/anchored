//go:build !windows

package ctx

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSandbox_ShellSuccess(t *testing.T) {
	sb := NewSandbox(5*time.Second, 1<<20, "")
	result, err := sb.Execute(context.Background(), "shell", "echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello")
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
}

func TestSandbox_Timeout(t *testing.T) {
	sb := NewSandbox(2*time.Second, 1<<20, "")
	result, err := sb.Execute(context.Background(), "shell", "sleep 30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if result.Duration >= 10*time.Second {
		t.Errorf("Duration = %v, should be well under 10s", result.Duration)
	}
}

func TestSandbox_OutputTruncation(t *testing.T) {
	sb := NewSandbox(5*time.Second, 100, "")
	result, err := sb.Execute(context.Background(), "shell",
		`for i in $(seq 1 1000); do echo "line $i"; done`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(result.Stdout) > 100 {
		t.Errorf("Stdout len = %d, want <= 100", len(result.Stdout))
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestSandbox_NonZeroExitCode(t *testing.T) {
	sb := NewSandbox(5*time.Second, 1<<20, "")
	result, err := sb.Execute(context.Background(), "shell", "exit 42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", result.ExitCode)
	}
}

func TestSandbox_PythonExecution(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	sb := NewSandbox(5*time.Second, 1<<20, "")
	result, err := sb.Execute(context.Background(), "python", `print("hello from python")`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello from python" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello from python")
	}
}

func TestSandbox_ContextCancellation(t *testing.T) {
	sb := NewSandbox(30*time.Second, 1<<20, "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1 * time.Second)
		cancel()
	}()

	result, err := sb.Execute(ctx, "shell", "sleep 30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Duration >= 10*time.Second {
		t.Errorf("Duration = %v, should be well under 10s", result.Duration)
	}
}

func TestSandbox_ConcurrentExecution(t *testing.T) {
	sb := NewSandbox(5*time.Second, 1<<20, "")
	var wg sync.WaitGroup
	results := make([]*ExecuteResult, 3)
	errors := make([]error, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			results[idx], errors[idx] = sb.Execute(
				context.Background(),
				"shell",
				`echo "concurrent `+string(rune('A'+idx))+`"`,
			)
		}()
	}
	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	for i, r := range results {
		if r == nil {
			t.Errorf("goroutine %d: nil result", i)
			continue
		}
		if r.ExitCode != 0 {
			t.Errorf("goroutine %d: ExitCode = %d, want 0", i, r.ExitCode)
		}
	}
}
