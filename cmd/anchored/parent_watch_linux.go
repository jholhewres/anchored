//go:build linux

package main

import (
	"log/slog"
	"runtime"

	"golang.org/x/sys/unix"
)

// setParentDeathSignal asks the kernel to deliver SIGTERM to this process the
// moment its parent dies. That signal flows through the same SIGTERM notifier
// runServe installs, so it triggers the normal graceful shutdown path with no
// polling latency.
//
// PR_SET_PDEATHSIG is a per-thread attribute delivered when the *setting*
// thread's parent dies, so we pin a dedicated goroutine to its OS thread and
// park it for the process lifetime to keep the setting alive under Go's M:N
// scheduler. This is best-effort — the getppid poll in watchParent is the
// authoritative mechanism and also covers the race where the parent already
// died before we got here — so any failure is logged at debug only.
func setParentDeathSignal(logger *slog.Logger) {
	go func() {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
			logger.Debug("PR_SET_PDEATHSIG failed, relying on getppid poll", "error", err)
			return
		}
		select {} // hold the locked thread so the setting survives
	}()
}
