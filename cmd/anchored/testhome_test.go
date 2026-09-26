package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain gives the package a throwaway HOME. Code paths that cannot load the
// config they are given fall back to the defaults, and the default database is
// ~/.anchored/data/anchored.db: before this, running the suite on a machine
// with anchored installed searched the live memory store, bumped injected_count
// on its memories and tightened its file modes.
func TestMain(m *testing.M) {
	if os.Getenv("ANCHORED_TEST_MAIN_ARGS") != "" {
		// TestMainHelper subprocess: the parent test set HOME, and main()
		// exits without returning here, so a temp dir made now would leak.
		os.Exit(m.Run())
	}
	home, err := os.MkdirTemp("", "anchored-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test home:", err)
		os.Exit(1)
	}
	for _, k := range []string{"HOME", "USERPROFILE"} {
		_ = os.Setenv(k, home)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestSuiteRunsUnderAThrowawayHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(home), "anchored-test-home-") {
		t.Fatalf("tests run with HOME=%s; TestMain must isolate it from the real ~/.anchored", home)
	}
}
