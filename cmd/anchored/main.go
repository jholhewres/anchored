package main

import (
	"fmt"
	"os"
)

var Version = "0.3.0"

func main() {
	if len(os.Args) < 2 {
		runServe()
		return
	}

	switch os.Args[1] {
	case "serve":
		runServe()
	case "import":
		runImport(os.Args[2:])
	case "search":
		runSearch(os.Args[2:])
	case "save":
		runSave(os.Args[2:])
	case "list":
		runList(os.Args[2:])
	case "forget":
		runForget(os.Args[2:])
	case "update":
		runUpdate(os.Args[2:])
	case "stats":
		runStats(os.Args[2:])
	case "identity":
		runIdentity(os.Args[2:])
	case "config":
		runConfig(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "precompact":
		runPrecompact(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "dream":
		runDream(os.Args[2:])
	case "--version", "-v":
		fmt.Printf("anchored %s\n", Version)
	case "--help", "-h":
		printUsage()
	default:
		runServe()
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "anchored %s — persistent cross-tool memory for AI coding agents\n\n", Version)
	fmt.Fprintf(os.Stderr, "Usage:\n")
	fmt.Fprintf(os.Stderr, "  anchored                    Start MCP server (STDIO)\n")
	fmt.Fprintf(os.Stderr, "  anchored serve              Start MCP server (STDIO)\n")
	fmt.Fprintf(os.Stderr, "  anchored import [sources]   Import memories from detected sources\n")
	fmt.Fprintf(os.Stderr, "  anchored search <query>     Search memories\n")
	fmt.Fprintf(os.Stderr, "  anchored save <content>     Save a memory\n")
	fmt.Fprintf(os.Stderr, "  anchored list               List memories\n")
	fmt.Fprintf(os.Stderr, "  anchored forget <id>        Remove a memory\n")
	fmt.Fprintf(os.Stderr, "  anchored update <id>        Update a memory\n")
	fmt.Fprintf(os.Stderr, "  anchored stats              Show memory statistics\n")
	fmt.Fprintf(os.Stderr, "  anchored identity [edit]    View or edit identity file\n")
	fmt.Fprintf(os.Stderr, "  anchored config [show|set]  View or modify configuration\n")
	fmt.Fprintf(os.Stderr, "  anchored init [--tool]     Initialize and register MCP server\n")
	fmt.Fprintf(os.Stderr, "  anchored precompact         Pre-compact memory context\n")
	fmt.Fprintf(os.Stderr, "  anchored hook <subcommand>  Run session continuity hooks\n")
	fmt.Fprintf(os.Stderr, "  anchored dream              Analyze and consolidate duplicate memories\n")
	fmt.Fprintf(os.Stderr, "  anchored --version          Print version\n")
	fmt.Fprintf(os.Stderr, "\nImport sources: claude-code devclaw opencode cursor all\n")
	fmt.Fprintf(os.Stderr, "\nFlags:\n")
	fmt.Fprintf(os.Stderr, "  --config <path>   Use specific config file\n")
}
