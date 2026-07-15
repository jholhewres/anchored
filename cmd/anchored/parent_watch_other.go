//go:build !linux

package main

import "log/slog"

// setParentDeathSignal is a no-op outside Linux, where PR_SET_PDEATHSIG has no
// portable equivalent. The getppid poll in watchParent still detects orphaning
// on every platform.
func setParentDeathSignal(_ *slog.Logger) {}
