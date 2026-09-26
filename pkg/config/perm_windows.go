//go:build windows

package config

import "os"

const noFollow = 0

func ownedByCaller(os.FileInfo) bool { return true }
