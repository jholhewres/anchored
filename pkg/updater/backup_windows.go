//go:build windows

package updater

import "os"

// backupCurrent moves dst aside so the staged binary can take its place.
//
// This is the opposite of the unix strategy on purpose. Windows maps a
// running executable as a section and refuses to delete or overwrite it, so
// MoveFileEx(new, dst, MOVEFILE_REPLACE_EXISTING) — which is what os.Rename
// compiles to — fails with ERROR_ACCESS_DENIED whenever dst is the .exe
// currently executing. And dst always is: self-update resolves its target
// from os.Executable.
//
// Renaming the running image itself is permitted, because it unlinks nothing.
// So the only sequence the loader allows is to vacate dst first and let the
// second rename create a fresh file at that name. That costs the atomicity
// the unix path gets from the hardlink: there is a brief moment where dst
// does not exist, and a client spawning `anchored serve` inside it sees the
// path missing. On Windows that window is unavoidable, and a backup that
// blocks every update is worse.
func backupCurrent(dst, prevPath string) error {
	return os.Rename(dst, prevPath)
}
