package main

import (
	"fmt"
	"os"
)

func runHook(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: anchored hook <subcommand> [options]")
		fmt.Fprintln(os.Stderr, "Subcommands: pretooluse, posttooluse, precompact, sessionstart")
		os.Exit(1)
	}
	switch args[0] {
	case "pretooluse":
		runHookPreToolUse(args[1:])
	case "posttooluse":
		runHookPostToolUse(args[1:])
	case "precompact":
		runHookPreCompact(args[1:])
	case "sessionstart":
		runHookSessionStart(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown hook subcommand: %s\n", args[0])
		os.Exit(1)
	}
}
