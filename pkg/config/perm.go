package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// PrivateFileMode is the mode for files that hold credentials or memory
// content: the config (remote API keys), the database and its WAL/SHM.
const PrivateFileMode os.FileMode = 0o600

// TightenPerm removes from path every permission bit want does not grant and
// reports whether it changed anything. It never adds a bit: a 0400 file stays
// 0400. A missing file, a directory, or a file owned by someone else is left
// alone. A symlinked path (dotfiles) is followed once, then the target is
// opened without following links and checked to be the same file before it is
// changed, so a link swapped in between cannot redirect the chmod. On Windows
// the POSIX bits do not map to access control, so it does nothing there.
func TightenPerm(path string, want os.FileMode) (bool, error) {
	if runtime.GOOS == "windows" {
		return false, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || !ownedByCaller(info) {
		return false, nil
	}
	current := info.Mode().Perm()
	next := current & want
	if next == current {
		return false, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(target, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }() // read-only descriptor: nothing to flush
	opened, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, opened) {
		return false, errors.New("file changed while tightening its permissions: " + path)
	}
	return true, f.Chmod(next)
}
