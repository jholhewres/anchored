//go:build !windows

package updater

import "os"

// osLink is a seam for tests. The degradation below is defined as "any Link
// failure", and the only honest way to assert that is to make Link fail with
// an errno the filesystem under the test will not produce on its own.
var osLink = os.Link

// backupCurrent links dst to prevPath so dst keeps existing for the whole
// swap — a client spawning `anchored serve` mid-update never finds the path
// missing, and the rename that follows is a true atomic replacement, dst
// going straight from the old inode to the new one.
//
// Any Link failure degrades to a rename. Hardlinks are missing or restricted
// on FAT, exFAT, several fuse and overlay mounts, and under some hardening
// policies, and each surface reports it with its own errno — enumerating them
// meant an unlisted one failed the update outright. The rename is exactly
// what this code did before the hardlink existed, so degrading costs only the
// brief window where dst is absent, which beats refusing to update at all.
func backupCurrent(dst, prevPath string) error {
	if err := osLink(dst, prevPath); err == nil {
		return nil
	}
	return backupByRename(dst, prevPath)
}
