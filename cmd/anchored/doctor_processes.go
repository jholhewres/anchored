package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

// dbHolder is a process on this host that has the database open.
type dbHolder struct {
	PID int
	Exe string
	// Replaced is true when the binary on disk changed after the process
	// started (a self-update or `make sync-bin`): it still runs the old code.
	Replaced bool
	Args     string
	RSSKB    int
}

// scanDBHolders lists the processes under procRoot (normally /proc) that hold
// dbPath, or its -wal/-shm, open, skipping self. It goes by open files rather
// than binary names because the self-update renames a running binary to
// anchored.prev, and because only a process with this database open matters.
// Processes of other users are skipped (their fds are unreadable). ok is false
// where there is no procfs (macOS, Windows): callers then rely on the registry.
func scanDBHolders(procRoot, dbPath string, self int) (holders []dbHolder, ok bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	targets := map[string]bool{}
	for _, p := range dbPathVariants(dbPath) {
		targets[p], targets[p+"-wal"], targets[p+"-shm"] = true, true, true
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		if !holdsAny(filepath.Join(dir, "fd"), targets) {
			continue
		}
		h := dbHolder{PID: pid}
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			h.Replaced = strings.HasSuffix(exe, " (deleted)")
			h.Exe = strings.TrimSuffix(exe, " (deleted)")
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
			h.Args = strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
		}
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			for _, line := range strings.Split(string(status), "\n") {
				if rest, found := strings.CutPrefix(line, "VmRSS:"); found {
					if fields := strings.Fields(rest); len(fields) > 0 {
						h.RSSKB, _ = strconv.Atoi(fields[0])
					}
				}
			}
		}
		holders = append(holders, h)
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].PID < holders[j].PID })
	return holders, true
}

// dbPathVariants returns dbPath as given and with symlinks resolved, since
// /proc reports the resolved path.
func dbPathVariants(dbPath string) []string {
	out := []string{filepath.Clean(dbPath)}
	if resolved, err := filepath.EvalSymlinks(dbPath); err == nil && resolved != out[0] {
		out = append(out, resolved)
	}
	return out
}

func holdsAny(fdDir string, targets map[string]bool) bool {
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join(fdDir, fd.Name())); err == nil && targets[target] {
			return true
		}
	}
	return false
}

// legacyBlockers names the processes that could run code older than
// minVersion against the database: registered ones below it (any host), and
// local holders that never registered unless they run the very binary file
// this process runs, which is the same code. A renamed or replaced binary, or
// another install, counts as older.
func legacyBlockers(live []memory.ProcessInfo, holders []dbHolder, host, selfExe, minVersion string) []string {
	var out []string
	registered := map[int]bool{}
	for _, p := range live {
		if p.Host == host {
			registered[p.PID] = true
		}
	}
	below := map[string]bool{}
	for _, p := range memory.ProcessesBelow(live, minVersion) {
		below[p.Host+"/"+strconv.Itoa(p.PID)] = true
	}
	type blocker struct {
		pid  int
		desc string
	}
	var found []blocker
	for _, p := range live {
		if below[p.Host+"/"+strconv.Itoa(p.PID)] {
			found = append(found, blocker{p.PID, fmt.Sprintf("pid %d (%s on %s, version %s)", p.PID, p.Role, p.Host, p.Version)})
		}
	}
	for _, h := range holders {
		if registered[h.PID] || (h.Exe == selfExe && !h.Replaced && selfExe != "") {
			continue
		}
		what := "unregistered"
		if h.Replaced {
			what = "binary replaced since start"
		}
		found = append(found, blocker{h.PID, fmt.Sprintf("pid %d (%s: %s)", h.PID, what, redactArgs(h.Args))})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].pid < found[j].pid })
	for _, b := range found {
		out = append(out, b.desc)
	}
	return out
}

// legacyHolders applies legacyBlockers to this host's registry and /proc.
func legacyHolders(ctx context.Context, store *memory.SQLiteStore, dbPath, minVersion string) ([]string, error) {
	live, err := store.LiveProcesses(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	holders, _ := scanDBHolders("/proc", dbPath, os.Getpid())
	return legacyBlockers(live, holders, host, currentExecutable(), minVersion), nil
}

func currentExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

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

// secretFlag matches flag names whose value must never be printed.
var secretFlag = regexp.MustCompile(`(?i)^--?[a-z0-9-]*(token|secret|password|passwd|api-?key|apikey)$`)

// redactArgs hides the values of secret-looking flags (--token X,
// --api-key=X, …) and drops control characters, so a process list can be
// shown, logged or handed to an agent without leaking credentials passed on
// the command line.
func redactArgs(args string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, args)
	fields := strings.Fields(clean)
	for i := 0; i < len(fields); i++ {
		name, _, hasValue := strings.Cut(fields[i], "=")
		if !secretFlag.MatchString(name) {
			continue
		}
		if hasValue {
			fields[i] = name + "=[REDACTED]"
		} else if i+1 < len(fields) {
			fields[i+1] = "[REDACTED]"
			i++
		}
	}
	return strings.Join(fields, " ")
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

	scanned, procfs := scanDBHolders("/proc", dbPath, os.Getpid())
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
			role, note = "cli", redactArgs(sp.Args)
		} else if filepath.Base(sp.Exe) == "anchored" {
			note = "unregistered: predates the registry"
		} else {
			role, note = filepath.Base(sp.Exe), redactArgs(sp.Args)
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

	holders, err := legacyHolders(ctx, store, dbPath, irreversibleStepsMinVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "legacy check: %v\n", err)
		return
	}
	if len(holders) > 0 {
		fmt.Printf("\n%d processes run code older than %s; upgrade steps that cannot be undone wait for them to exit:\n",
			len(holders), irreversibleStepsMinVersion)
		for _, h := range holders {
			fmt.Printf("  %s\n", h)
		}
		fmt.Println("Close and reopen those editor sessions (or restart the hub) to clear them.")
	}
}

// irreversibleStepsMinVersion is the oldest binary allowed to hold the
// database while a one-way step runs (activating a new embedding space,
// dropping a legacy column).
const irreversibleStepsMinVersion = "0.20.0"
