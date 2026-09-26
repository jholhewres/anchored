package project

import (
	"path/filepath"
	"testing"
)

// realTempDir is t.TempDir with symlinks resolved. The detector canonicalizes
// paths, and on macOS the temp dir lives under /var, a symlink to
// /private/var: rows written with the raw path never match what it looks up.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
