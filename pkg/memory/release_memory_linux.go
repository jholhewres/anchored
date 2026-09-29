//go:build linux && cgo

package memory

/*
#include <stddef.h>

// Weak, so a libc without malloc_trim (musl) links and skips the call.
extern int malloc_trim(size_t pad) __attribute__((weak));

static void anchored_malloc_trim(void) {
	if (malloc_trim) {
		malloc_trim(0);
	}
}
*/
import "C"

import "runtime/debug"

// releaseMemoryToOS hands freed memory back to the system: the Go heap's and
// the C allocator's. glibc keeps what the ONNX runtime frees in its arenas, so
// without the trim a process that loads and unloads the model grows by about
// 50 MB per cycle.
func releaseMemoryToOS() {
	debug.FreeOSMemory()
	C.anchored_malloc_trim()
}
