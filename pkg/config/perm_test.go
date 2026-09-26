//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTightenPerm_OnlyRemovesBits(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		start, want os.FileMode
		changed     bool
	}{
		{0o664, 0o600, true},
		{0o644, 0o600, true},
		{0o600, 0o600, false},
		{0o400, 0o400, false}, // never widens
	}
	for i, c := range cases {
		p := filepath.Join(dir, string(rune('a'+i)))
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, c.start); err != nil {
			t.Fatal(err)
		}
		changed, err := TightenPerm(p, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(p)
		if info.Mode().Perm() != c.want || changed != c.changed {
			t.Errorf("%o: got %o changed=%v, want %o changed=%v", c.start, info.Mode().Perm(), changed, c.want, c.changed)
		}
	}
	if changed, err := TightenPerm(filepath.Join(dir, "missing"), 0o600); err != nil || changed {
		t.Errorf("a missing file is not an error: changed=%v err=%v", changed, err)
	}
}

// The config holds remote API keys; loading it tightens a mode left open by
// an older writer or a manual edit.
func TestLoad_TightensConfigMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("memory:\n  database_path: /tmp/x.db\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode after Load = %o, want 600", info.Mode().Perm())
	}
}

// config.yaml is often a symlink into a dotfiles repo: the file it names holds
// the keys, so that is the one tightened, and the link itself stays a link.
func TestTightenPerm_TightensTheFileASymlinkNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	changed, err := TightenPerm(link, 0o600)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	info, _ := os.Stat(target)
	linfo, _ := os.Lstat(link)
	if info.Mode().Perm() != 0o600 || linfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("target %o, link mode %v", info.Mode().Perm(), linfo.Mode())
	}
}

// database_path can name a directory by mistake; tightening it to 0600 would
// lock the owner out of it.
func TestTightenPerm_LeavesDirectoriesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := TightenPerm(dir, 0o600)
	info, _ := os.Stat(dir)
	if err != nil || changed || info.Mode().Perm() != 0o755 {
		t.Errorf("directory: changed=%v err=%v mode %o", changed, err, info.Mode().Perm())
	}
}
