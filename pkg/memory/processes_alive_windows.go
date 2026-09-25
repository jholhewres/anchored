//go:build windows

package memory

// processAlive cannot probe a pid cheaply on Windows; the registry falls back
// to heartbeat staleness alone there.
func processAlive(pid int) bool { return pid > 0 }
