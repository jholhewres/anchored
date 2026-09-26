//go:build !windows

package config

import (
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func ownedByCaller(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}
