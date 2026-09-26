package memory

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

	"github.com/jholhewres/anchored/pkg/redact"
)

// IrreversibleStepsMinVersion is the oldest binary allowed to hold the
// database while a one-way step runs (activating a new embedding space,
// dropping a legacy column).
const IrreversibleStepsMinVersion = "0.20.0"

// DBHolder is a process on this host that has the database open.
type DBHolder struct {
	PID int
	Exe string
	// Replaced is true when the binary on disk changed after the process
	// started (a self-update or `make sync-bin`): it still runs the old code.
	Replaced bool
	Args     string
	RSSKB    int
}

// ScanDBHolders lists the processes under procRoot (normally /proc) that hold
// dbPath, or its -wal/-shm, open, skipping self. It goes by open files rather
// than binary names because the self-update renames a running binary to
// anchored.prev, and because only a process with this database open matters.
// Processes of other users are skipped (their fds are unreadable). ok is false
// where there is no procfs (macOS, Windows): callers then rely on the registry.
func ScanDBHolders(procRoot, dbPath string, self int) (holders []DBHolder, ok bool) {
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
		h := DBHolder{PID: pid}
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

// LegacyBlockers names the processes that could run code older than
// minVersion against the database: registered ones below it (any host), and
// local holders that never registered unless they run the very binary file
// this process runs, which is the same code. A renamed or replaced binary, or
// another install, counts as older.
func LegacyBlockers(live []ProcessInfo, holders []DBHolder, host, selfExe, minVersion string) []string {
	var out []string
	registered := map[int]bool{}
	for _, p := range live {
		if p.Host == host {
			registered[p.PID] = true
		}
	}
	below := map[string]bool{}
	for _, p := range ProcessesBelow(live, minVersion) {
		below[p.Host+"/"+strconv.Itoa(p.PID)] = true
	}
	type blocker struct {
		pid  int
		desc string
	}
	var found []blocker
	for _, p := range live {
		if below[p.Host+"/"+strconv.Itoa(p.PID)] {
			found = append(found, blocker{p.PID, fmt.Sprintf("pid %d (%s on %s, version %s)",
				p.PID, stripControl(p.Role), stripControl(p.Host), stripControl(p.Version))})
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
		found = append(found, blocker{h.PID, fmt.Sprintf("pid %d (%s: %s)", h.PID, what, RedactArgs(h.Args))})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].pid < found[j].pid })
	for _, b := range found {
		out = append(out, b.desc)
	}
	return out
}

// LegacyHolders applies LegacyBlockers to this host's registry and /proc: the
// processes that must be gone before a step older code cannot follow (the
// activation of an embedding generation it cannot query, a dropped column).
func (s *SQLiteStore) LegacyHolders(ctx context.Context, dbPath, minVersion string) ([]string, error) {
	holders, _, err := s.LegacyHoldersVerified(ctx, dbPath, minVersion)
	return holders, err
}

// procRoot is where LegacyHoldersVerified looks for processes (tests swap it).
var procRoot = "/proc"

// LegacyHoldersVerified is LegacyHolders, also reporting whether the holders
// of the database could be enumerated at all: without procfs only registered
// processes are seen, and binaries before v0.20 never register.
func (s *SQLiteStore) LegacyHoldersVerified(ctx context.Context, dbPath, minVersion string) ([]string, bool, error) {
	live, err := s.LiveProcesses(ctx, time.Now())
	if err != nil {
		return nil, false, err
	}
	host, _ := os.Hostname()
	holders, verified := ScanDBHolders(procRoot, dbPath, os.Getpid())
	return LegacyBlockers(live, holders, host, CurrentExecutable(), minVersion), verified, nil
}

func CurrentExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// secretFlag matches flag names whose value must never be printed.
var secretFlag = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(token|secret|password|passwd|pass|pwd|key|auth|credentials?|cookie|bearer)$`)

// secretName matches NAME=VALUE assignments with a secret-looking name
// (ANCHORED_TOKEN=…, PGPASSWORD=…).
var secretName = regexp.MustCompile(`(?i)^[a-z0-9_]*(token|secret|password|passwd|pass|pwd|key|auth|credentials?|cookie|bearer)$`)

// stripControl drops control characters, so text from a process list or the
// registry cannot carry terminal escapes.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// RedactArgs hides the values of secret-looking flags (--token X,
// --api-key=X, …) and drops control characters, so a process list can be
// shown, logged or handed to an agent without leaking credentials passed on
// the command line.
func RedactArgs(args string) string {
	fields := strings.Fields(stripControl(args))
	for i := 0; i < len(fields); i++ {
		name, _, hasValue := strings.Cut(fields[i], "=")
		switch {
		case secretFlag.MatchString(name):
			if hasValue {
				fields[i] = name + "=[REDACTED]"
			} else if i+1 < len(fields) {
				fields[i+1] = "[REDACTED]"
				i++
			}
		case hasValue && secretName.MatchString(name):
			fields[i] = name + "=[REDACTED]"
		}
	}
	// Whatever else looks like a credential (URL userinfo, bearer tokens,
	// provider key formats) goes through the shared redaction rules.
	return redact.String(strings.Join(fields, " "))
}
