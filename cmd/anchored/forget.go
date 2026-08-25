package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/memory"
)

func runForget(args []string) {
	fs := newFlagSet("forget")
	configPath := fs.String("config", "", "path to config file")
	hard := fs.Bool("hard", false, "permanently delete (default: soft delete)")
	category := fs.String("category", "", "bulk: restrict to a category")
	source := fs.String("source", "", "bulk: restrict to a source")
	project := fs.String("project", "", "bulk: restrict to a project id")
	olderThan := fs.String("older-than", "", "bulk: only memories created before this age (e.g. 60d, 12h)")
	limit := fs.Int("limit", 0, "bulk: cap how many memories one run removes (0 = no cap)")
	yes := fs.Bool("yes", false, "bulk: actually delete (without it, bulk mode only reports the count)")
	fs.Parse(args)

	id := fs.Arg(0)
	bulk := *category != "" || *source != "" || *project != "" || *olderThan != ""

	if id == "" && !bulk {
		fmt.Fprintln(os.Stderr, "Usage:")
		fmt.Fprintln(os.Stderr, "  anchored forget <id> [--hard]")
		fmt.Fprintln(os.Stderr, "  anchored forget --category C [--older-than 60d] [--source S] [--project P] [--limit N] [--hard] --yes")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Bulk mode reports the match count and deletes nothing until --yes is passed.")
		os.Exit(1)
	}
	if id != "" && bulk {
		fmt.Fprintln(os.Stderr, "forget error: pass either an id or bulk filters, not both")
		os.Exit(1)
	}

	var cutoff time.Time
	if *olderThan != "" {
		age, err := parseAge(*olderThan)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
			os.Exit(1)
		}
		cutoff = time.Now().UTC().Add(-age)
	}

	_, _, svc, err := initService(*configPath)
	if err != nil {
		slog.Error("failed to initialize", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	ctx := context.Background()

	if bulk {
		opts := memory.DeleteScopeOptions{
			ProjectID: *project,
			Category:  *category,
			Source:    *source,
			Hard:      *hard,
			OlderThan: cutoff,
			Limit:     *limit,
			DryRun:    !*yes,
		}
		mode := "soft-delete"
		if *hard {
			mode = "permanent delete"
		}

		if opts.DryRun {
			// Report the true match count alongside the capped one. With --limit
			// the capped number alone reads as "that is all there is", which is
			// the opposite of what it means.
			capped, err := svc.ForgetScope(ctx, opts)
			if err != nil {
				fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
				os.Exit(1)
			}
			uncapped := capped
			if opts.Limit > 0 {
				noLimit := opts
				noLimit.Limit = 0
				if total, terr := svc.ForgetScope(ctx, noLimit); terr == nil {
					uncapped = total
				}
			}
			if uncapped != capped {
				fmt.Printf("%d memories match; %d would be removed (%s, --limit %d). Nothing deleted — pass --yes to apply.\n",
					uncapped, capped, mode, opts.Limit)
			} else {
				fmt.Printf("%d memories match (%s). Nothing deleted — pass --yes to apply.\n", capped, mode)
			}
			return
		}

		n, err := svc.ForgetScope(ctx, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%d memories removed (%s).\n", n, mode)
		return
	}

	if *hard {
		if err := svc.Forget(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Permanently deleted memory %s\n", id)
		return
	}
	if err := svc.SoftForget(ctx, id); err != nil {
		fmt.Fprintf(os.Stderr, "forget error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Soft-deleted memory %s\n", id)
}

// maxAgeDays bounds --older-than. Past this a time.Duration overflows int64 and
// wraps negative, which turns `now - age` into a cutoff in the FUTURE that
// matches every row — so `--older-than 106752d --hard --yes` would delete the
// whole corpus. The realistic way to hit it is a script passing epoch seconds
// where days are expected. A century is beyond any retention policy; anything
// larger is a typo, and a typo on a destructive command must be rejected, not
// silently reinterpreted.
const maxAgeDays = 36500

// parseAge accepts the durations a retention cut is actually expressed in.
// time.ParseDuration tops out at hours, so days get their own suffix.
func parseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if raw, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(raw)
		if err != nil || days <= 0 || days > maxAgeDays {
			return 0, fmt.Errorf("invalid age %q: want 1d..%dd", s, maxAgeDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	// A zero age puts the cutoff at now, silently matching everything — the same
	// hazard as the overflow, reached by a much likelier typo.
	if err != nil || d <= 0 || d > maxAgeDays*24*time.Hour {
		return 0, fmt.Errorf("invalid age %q: want something like 60d or 12h (max %dd)", s, maxAgeDays)
	}
	return d, nil
}
