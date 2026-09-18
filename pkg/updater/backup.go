package updater

import "os"

// backupByRename moves dst aside instead of copying it. It is the windows
// strategy outright and the unix fallback when a hardlink is not available,
// and it lives here, untagged, so both platforms and the tests use one
// implementation rather than three copies of the same rename.
//
// The cost is that dst stops existing until the staged binary is renamed over
// it. A client spawning `anchored serve` inside that window finds nothing at
// the path.
func backupByRename(dst, prevPath string) error {
	return os.Rename(dst, prevPath)
}
