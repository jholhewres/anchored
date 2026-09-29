//go:build !linux || !cgo

package memory

import "runtime/debug"

// releaseMemoryToOS hands the Go heap's freed memory back to the system.
func releaseMemoryToOS() {
	debug.FreeOSMemory()
}
