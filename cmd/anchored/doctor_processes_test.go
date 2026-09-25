package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/memory"
)

func fakeProc(t *testing.T, root string, pid int, exe, cmdline, status string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsShortLivedInvocation(t *testing.T) {
	cases := map[string]bool{
		"anchored":                               false,
		"anchored serve --stdio":                 false,
		"anchored --config /x/config.yaml":       false,
		"anchored dashboard --no-open --addr x":  false,
		"anchored hub serve":                     false,
		"anchored hook stop":                     true,
		"anchored maintenance run":               true,
		"anchored search foo":                    true,
		"anchored --config /x/c.yaml search foo": true,
		"anchored --config=/x/c.yaml hook stop":  true,
		"anchored --config /x/c.yaml":            false,
	}
	for args, want := range cases {
		if got := isShortLivedInvocation(args); got != want {
			t.Errorf("%q: got %v, want %v", args, got, want)
		}
	}
}

func fakeFD(t *testing.T, root string, pid, fd int, target string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid), "fd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, strconv.Itoa(fd))); err != nil {
		t.Fatal(err)
	}
}

// The self-update renames the running binary to anchored.prev, so a process
// is recognised by holding the database open, never by its binary's name.
func TestScanDBHolders_FindsHoldersByOpenFile(t *testing.T) {
	root := t.TempDir()
	db := "/home/u/.anchored/data/anchored.db"
	fakeProc(t, root, 100, "/home/u/.anchored/bin/anchored.prev (deleted)", "anchored\x00", "VmRSS:\t 2048 kB\n")
	fakeFD(t, root, 100, 3, db)
	fakeProc(t, root, 101, "/home/u/.anchored/bin/anchored", "anchored\x00", "")
	fakeFD(t, root, 101, 3, "/other/project.db")
	fakeProc(t, root, 102, "/usr/bin/sqlite3", "sqlite3\x00", "")
	fakeFD(t, root, 102, 5, db+"-wal")
	fakeProc(t, root, 103, "/home/u/.anchored/bin/anchored", "anchored\x00", "") // no fd dir

	holders, ok := scanDBHolders(root, db, 0)
	if !ok {
		t.Fatal("a readable proc root must report ok")
	}
	if len(holders) != 2 || holders[0].PID != 100 || holders[1].PID != 102 {
		t.Fatalf("expected pids 100 and 102 to hold the database, got %+v", holders)
	}
	if !holders[0].Replaced || holders[0].Exe != "/home/u/.anchored/bin/anchored.prev" || holders[0].RSSKB != 2048 {
		t.Errorf("renamed binary must be reported with its cleaned path: %+v", holders[0])
	}
}

func TestLegacyBlockers_TrustOnlyRegisteredOrSameBinary(t *testing.T) {
	self := "/home/u/.anchored/bin/anchored"
	live := []memory.ProcessInfo{
		{PID: 10, Host: "h", Version: "0.20.0", Role: "serve"},
		{PID: 11, Host: "h", Version: "0.19.2", Role: "serve"},
		{PID: 12, Host: "other", Version: "0.19.2", Role: "hub"},
	}
	holders := []dbHolder{
		{PID: 10, Exe: self},                                              // registered, current
		{PID: 11, Exe: self},                                              // registered, old version
		{PID: 20, Exe: self, Args: "anchored hook stop"},                  // same binary file: current code
		{PID: 21, Exe: self + ".prev", Replaced: true, Args: "anchored"},  // renamed by self-update
		{PID: 22, Exe: "/usr/local/bin/anchored", Args: "anchored serve"}, // another install
	}
	got := legacyBlockers(live, holders, "h", self, "0.20.0")
	want := []int{11, 12, 21, 22}
	if len(got) != len(want) {
		t.Fatalf("blockers: got %v, want pids %v", got, want)
	}
	for i, pid := range want {
		if !strings.Contains(got[i], "pid "+strconv.Itoa(pid)+" ") {
			t.Errorf("blocker %d: got %q, want pid %d", i, got[i], pid)
		}
	}
}

func TestRedactArgs_HidesSecretFlagValues(t *testing.T) {
	cases := map[string]string{
		"anchored hub --allow-remote --token S3CRET":         "anchored hub --allow-remote --token [REDACTED]",
		"anchored migrate --token=S3CRET --dry-run":          "anchored migrate --token=[REDACTED] --dry-run",
		"anchored remote add --api-key K1 --name x":          "anchored remote add --api-key [REDACTED] --name x",
		"anchored x --write-token T --password P --secret=S": "anchored x --write-token [REDACTED] --password [REDACTED] --secret=[REDACTED]",
		"anchored serve --config /x/config.yaml":             "anchored serve --config /x/config.yaml",
		"anchored search \x1b[31mred\x07":                    "anchored search [31mred",
	}
	for in, want := range cases {
		if got := redactArgs(in); got != want {
			t.Errorf("redactArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
