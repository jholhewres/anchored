package main

import (
	"fmt"
	"os"
)

// Version is overridden at build time via `-ldflags -X main.Version=$(cat VERSION)`
// (see Makefile and .goreleaser.yaml). The "dev" placeholder is what you see
// when running `go build` directly without the Makefile target.
var Version = "dev"

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
	case "directives":
		runDirectives(os.Args[2:])
	case "task":
		runTask(os.Args[2:])
	case "config":
		runConfig(os.Args[2:])
	case "init":
		runInit(os.Args[2:])
	case "precompact":
		runPrecompact(os.Args[2:])
	case "handoff":
		runHandoff(os.Args[2:])
	case "bootstrap":
		runBootstrap(os.Args[2:])
	case "retention":
		runRetention(os.Args[2:])
	case "hook":
		runHook(os.Args[2:])
	case "dream":
		runDream(os.Args[2:])
	case "curation":
		runCuration(os.Args[2:])
	case "doctor":
		runDoctor(os.Args[2:])
	case "purge":
		runPurge(os.Args[2:])
	case "inspect":
		runInspect(os.Args[2:])
	case "export":
		runExport(os.Args[2:])
	case "artifact":
		runArtifact(os.Args[2:])
	case "remote":
		runRemote(os.Args[2:])
	case "eval":
		runEval(os.Args[2:])
	case "backfill":
		runBackfillEmbeddings(os.Args[2:])
	case "dashboard":
		runDashboard(os.Args[2:])
	case "maintenance":
		runMaintenance(os.Args[2:])
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
	fmt.Fprintf(os.Stderr, "  anchored config [show|set|wizard] View or modify configuration\n")
	fmt.Fprintf(os.Stderr, "  anchored init [--tool]     Initialize and register MCP server\n")
	fmt.Fprintf(os.Stderr, "  anchored precompact         Pre-compact memory context\n")
	fmt.Fprintf(os.Stderr, "  anchored handoff            Save a handoff snapshot for session continuity\n")
	fmt.Fprintf(os.Stderr, "  anchored bootstrap          Bootstrap project memories from sources\n")
	fmt.Fprintf(os.Stderr, "  anchored retention sweep    Sweep expired/episodic memories\n")
	fmt.Fprintf(os.Stderr, "  anchored hook <subcommand>  Run session continuity hooks\n")
	fmt.Fprintf(os.Stderr, "  anchored dream              Analyze and consolidate duplicate memories\n")
	fmt.Fprintf(os.Stderr, "  anchored curation status    Show curation worker state\n")
	fmt.Fprintf(os.Stderr, "  anchored curation disable   Disable serve-time curation worker\n")
	fmt.Fprintf(os.Stderr, "  anchored curation score     Score and mark low-signal memories\n")
	fmt.Fprintf(os.Stderr, "  anchored curation reconcile Re-score entire corpus, repair stale flags\n")
	fmt.Fprintf(os.Stderr, "  anchored doctor             Diagnose installation, config, MCP registration\n")
	fmt.Fprintf(os.Stderr, "  anchored purge              Wipe memories (--hard for full DB reset)\n")
	fmt.Fprintf(os.Stderr, "  anchored inspect <id>      Show full memory details\n")
	fmt.Fprintf(os.Stderr, "  anchored export             Export memories (JSON/JSONL)\n")
	fmt.Fprintf(os.Stderr, "  anchored artifact add       Index content as an artifact (--type, --file, --label)\n")
	fmt.Fprintf(os.Stderr, "  anchored artifact search    FTS search over indexed artifacts\n")
	fmt.Fprintf(os.Stderr, "  anchored artifact list      List recent artifacts\n")
	fmt.Fprintf(os.Stderr, "  anchored artifact prune     Remove expired artifacts\n")
	fmt.Fprintf(os.Stderr, "  anchored remote status      Show remote sync config status\n")
	fmt.Fprintf(os.Stderr, "  anchored remote preview     Preview which memories would sync (offline)\n")
	fmt.Fprintf(os.Stderr, "  anchored dashboard [--addr] Start local web dashboard (stats, search, KG, health)\n")
	fmt.Fprintf(os.Stderr, "  anchored dashboard install  Install dashboard as a systemd --user service\n")
	fmt.Fprintf(os.Stderr, "  anchored maintenance run    Periodic upkeep: import + dream + curation reconcile\n")
	fmt.Fprintf(os.Stderr, "  anchored maintenance install  Install upkeep as a systemd --user daily timer\n")
	fmt.Fprintf(os.Stderr, "  anchored --version          Print version\n")
	fmt.Fprintf(os.Stderr, "\nImport sources: claude-code devclaw opencode cursor all\n")
	fmt.Fprintf(os.Stderr, "\nFlags:\n")
	fmt.Fprintf(os.Stderr, "  --config <path>   Use specific config file\n")
}
