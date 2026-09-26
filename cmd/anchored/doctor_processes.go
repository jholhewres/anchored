package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// isShortLivedInvocation reports whether args are a hook or one-shot CLI run
// rather than a long-lived server: those finish on their own and do not
// register. Leading global flags (--config PATH) are skipped to find the
// subcommand; no subcommand means the bare MCP server.
func isShortLivedInvocation(args string) bool {
	fields := strings.Fields(args)
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "-") {
			if (f == "--config" || f == "-config") && i+1 < len(fields) {
				i++
			}
			continue
		}
		switch f {
		case "serve", "hub", "dashboard":
			return false
		}
		return true
	}
	return false
}

func runDoctorProcesses(dbPath string) {
	store, err := memory.NewSQLiteStore(dbPath, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx := context.Background()
	live, err := store.LiveProcesses(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "process registry: %v\n", err)
		os.Exit(1)
	}
	host, _ := os.Hostname()
	byPID := map[int]memory.ProcessInfo{}
	for _, p := range live {
		if p.Host == host {
			byPID[p.PID] = p
		}
	}

	scanned, procfs := memory.ScanDBHolders("/proc", dbPath, os.Getpid())
	fmt.Printf("%-8s %-10s %-24s %8s %8s  %s\n", "PID", "ROLE", "VERSION", "RSS", "UPTIME", "NOTE")
	totalKB := 0
	seen := map[int]bool{}
	for _, sp := range scanned {
		seen[sp.PID] = true
		totalKB += sp.RSSKB
		role, version, uptime, note := "?", "?", "", ""
		if rp, ok := byPID[sp.PID]; ok {
			role, version = rp.Role, rp.Version
			uptime = time.Since(rp.StartedAt).Round(time.Minute).String()
		} else if isShortLivedInvocation(sp.Args) {
			role, note = "cli", memory.RedactArgs(sp.Args)
		} else if filepath.Base(sp.Exe) == "anchored" {
			note = "unregistered: predates the registry"
		} else {
			role, note = filepath.Base(sp.Exe), memory.RedactArgs(sp.Args)
		}
		if sp.Replaced {
			note = strings.TrimSpace(note + " binary replaced since start")
		}
		fmt.Printf("%-8d %-10s %-24s %7dM %8s  %s\n", sp.PID, role, version, sp.RSSKB/1024, uptime, note)
	}
	for _, rp := range live {
		if rp.Host == host && procfs && seen[rp.PID] {
			continue
		}
		fmt.Printf("%-8d %-10s %-24s %8s %8s  %s\n", rp.PID, rp.Role, rp.Version, "-",
			time.Since(rp.StartedAt).Round(time.Minute).String(), "host "+rp.Host)
	}
	if procfs {
		fmt.Printf("\n%d processes hold the database, %d MB resident\n", len(scanned), totalKB/1024)
	} else {
		fmt.Println("\n(no /proc on this system: only registered processes are listed)")
	}

	holders, verified, err := store.LegacyHoldersVerified(ctx, dbPath, memory.IrreversibleStepsMinVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "legacy check: %v\n", err)
		return
	}
	if !verified {
		fmt.Printf("\nThis platform has no /proc: only registered processes are listed, and binaries before %s never register.\n"+
			"Once every client runs %s or newer, set embedding.confirm_upgrade: true so the embedding upgrade can switch.\n",
			memory.IrreversibleStepsMinVersion, memory.IrreversibleStepsMinVersion)
	}
	if len(holders) > 0 {
		fmt.Printf("\n%d processes run code older than %s; upgrade steps that cannot be undone wait for them to exit:\n",
			len(holders), memory.IrreversibleStepsMinVersion)
		for _, h := range holders {
			fmt.Printf("  %s\n", h)
		}
		fmt.Println("Close and reopen those editor sessions (or restart the hub) to clear them.")
	}
}
