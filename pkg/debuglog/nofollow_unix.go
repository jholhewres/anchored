//go:build !windows

package debuglog

import "syscall"

// noFollow makes an open fail on a symlink at the log path.
const noFollow = syscall.O_NOFOLLOW
