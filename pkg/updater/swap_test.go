//go:build !windows

package updater

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func stageFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Windows cannot replace a running .exe by renaming over it — the image
// loader refuses to let the file be deleted. backup_windows.go therefore
// vacates dst first, and this drives that exact sequence on Linux, where CI
// runs, so the ordering stays covered even though the platform is not.
func TestSwapUsing_RenameBackupVacatesDstFirst(t *testing.T) {
	dir := t.TempDir()
	dst := stageFile(t, dir, "anchored", "OLD")
	tmpPath := stageFile(t, dir, ".anchored-new-x", "NEW")

	if err := swapUsing(tmpPath, dst, backupByRename); err != nil {
		t.Fatalf("swapUsing: %v", err)
	}

	if got, _ := os.ReadFile(dst); string(got) != "NEW" {
		t.Errorf("dst = %q, want NEW", got)
	}
	if got, _ := os.ReadFile(dst + ".prev"); string(got) != "OLD" {
		t.Errorf(".prev = %q, want OLD", got)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("staging file %s survived the swap", tmpPath)
	}
}

// The rename strategy leaves dst absent between the two renames, so a failure
// in the second one must put the old binary back. Without the rollback the
// user is left with no executable at all — the worst outcome this package can
// produce.
func TestSwapUsing_RenameBackupRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := stageFile(t, dir, "anchored", "OLD")
	// A staging path that does not exist makes the second rename fail with
	// ENOENT after the backup has already moved dst away.
	tmpPath := filepath.Join(dir, ".anchored-new-missing")

	err := swapUsing(tmpPath, dst, backupByRename)
	if err == nil {
		t.Fatal("expected the swap to fail")
	}

	if got, _ := os.ReadFile(dst); string(got) != "OLD" {
		t.Errorf("dst = %q after rollback, want the original OLD", got)
	}
	if _, statErr := os.Stat(dst + ".prev"); statErr == nil {
		t.Error(".prev survived a rolled-back swap, so a later run would promote a stale binary")
	}
}

// The unix strategy keeps dst in place, which is what makes the final rename
// an atomic swap. Asserted by inode, not by content: a rename-based backup
// would also leave both files readable.
func TestSwapUsing_HardlinkBackupKeepsDstPresent(t *testing.T) {
	dir := t.TempDir()
	dst := stageFile(t, dir, "anchored", "OLD")
	tmpPath := stageFile(t, dir, ".anchored-new-y", "NEW")

	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := swapUsing(tmpPath, dst, backupCurrent); err != nil {
		t.Fatalf("swapUsing: %v", err)
	}

	prev, err := os.Stat(dst + ".prev")
	if err != nil {
		t.Fatalf("stat .prev: %v", err)
	}
	if !os.SameFile(before, prev) {
		t.Error(".prev is not the original inode, so the backup was not a hardlink")
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW" {
		t.Errorf("dst = %q, want NEW", got)
	}
}

// The fallback is defined as "any Link failure", not as a list of errnos: the
// filesystems that lack or restrict hardlinks each report it differently, and
// the enumerated version failed the update outright on an unlisted one. EACCES
// is deliberately a code the old allowlist (ErrUnsupported, EXDEV, EPERM) did
// not cover, so this test fails if the list comes back.
func TestBackupCurrent_AnyLinkFailureDegradesToRename(t *testing.T) {
	dir := t.TempDir()
	dst := stageFile(t, dir, "anchored", "OLD")
	prevPath := dst + ".prev"

	orig := osLink
	osLink = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EACCES} }
	t.Cleanup(func() { osLink = orig })

	if err := backupCurrent(dst, prevPath); err != nil {
		t.Fatalf("backupCurrent should have degraded to a rename, got %v", err)
	}
	if got, _ := os.ReadFile(prevPath); string(got) != "OLD" {
		t.Errorf(".prev = %q, want OLD", got)
	}
}
