# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

Session Cockpit (first slice): see every live coding session and bind one to a
task on your personal board.

### Added

- **`anchored hub`** — an always-on local daemon that serves the dashboard and
  runs upkeep workers (ending stale sessions, pruning old events) on a ticker,
  so the live view stays honest even with no tool connected. `hub
  install/uninstall/status` manage a systemd --user service; the per-tool
  `serve --stdio` processes stay lightweight producers.
- **Live sessions** — sessions now record provider/model/intent (telling Claude
  Code on Anthropic apart from Claude Code + GLM, detected from the
  environment). `GET /api/sessions/live` and a new **Cockpit** dashboard tab
  show each open session with its tool, project, intent, recent activity and
  active/idle state.
- **Session ↔ task link** — bind a live session to a board task (1 session → 1
  task; a task accumulates many sessions), create a task from a session, or
  unlink, via the cockpit and `POST/DELETE /api/sessions/{id}/link` +
  `/promote-task`. Notable session events flow into the linked task's journal;
  `/api/tasks` now reports a `live_session_id` when a session is active on it.

### Fixed

- **Dev-stamped local builds** — `make build` binaries now report
  `v0.17.0-dev+g<hash>[.dirty]` instead of the bare release semver, so a
  local checkout install is never mistaken for the published tag. Plugin
  manifests and goreleaser keep the clean version. Autoupdate now skips dev
  builds entirely — previously a local install under `~/.anchored/bin` could
  be silently reverted to the release tag on the next `serve` start — and
  plugin drift detection treats them like the bare `dev` placeholder.

## [0.17.0] - 2026-08-26

Bulk pruning for `anchored forget`, and a set of fixes for paths that were
losing data or silently doing nothing.

### Added

- **Bulk `anchored forget`** — prune by `--category`, `--older-than`,
  `--project` and `--source` instead of one id per process. Removing an
  imported category used to mean one spawn (and one ONNX load) per memory.
  Bulk mode reports the match count and deletes nothing by default; `--yes`
  is required to act, `--limit` drains oldest-first, and `--older-than` is
  bounded to 1d..36500d so a typo on a destructive command is refused rather
  than reinterpreted.

### Fixed

- **`--limit` was ignored by search** — `HybridSearcher.Search` took its limit
  from `search.max_results` regardless of the per-call option, so
  `anchored search --limit 100` returned the configured default (20) and no
  caller could look deeper. The per-call value now wins and the config is what
  it reads like: a default for callers that do not ask.
- **Data-loss paths in bulk forget** — `parseAge` overflowed `time.Duration`
  past 106752d and produced a cutoff in the *future*, matching every row;
  `DeleteByScope` resolved its target ids outside the transaction that deleted
  them, so a concurrent update could move a memory and still have it deleted
  under its old category; and `dream_actions`, which references memories
  through two unconstrained columns, was missing from satellite cleanup and
  left an orphan per action per pruned memory.
- **Stale embedding vectors were never restamped** — `PutEmbeddingVector`
  gated its `DO UPDATE` on the stored `content_hash` already equalling the
  incoming one, so a row with a stale stamp could not be corrected: the
  conflict fired, the update was dropped, and the call returned success. The
  upsert now sets `content_hash` and a dropped write is an error.
- **Artifact capture ran with the optimizer switched off** — `posttooluse`
  indexed every large tool output into `content_chunks`, but the evictor that
  reclaims those rows is constructed only when `context_optimizer.enabled` is
  on. Turning the optimizer off left the writes running with nothing acting on
  the 72h TTL — on one machine 23,198 of 23,617 chunk rows were expired, about
  300 MB. Capture is now gated on the same flag as eviction.
- **Dream aborted on a single unhashed row** — `Analyze` scanned the nullable
  `memories.content_hash` into a plain string, so one NULL killed the whole
  consolidation pass and `anchored maintenance run` failed hard. Unhashed rows
  now fall out of exact dedup and are still considered by the semantic tiers.
- **`--help` crashed subcommands** — `reorderArgsForFlag` dereferenced the
  result of `fs.Lookup` before its own nil check, so `anchored search --help`
  and every other reordering subcommand died with SIGSEGV instead of printing
  usage.

## [0.16.0] - 2026-07-24

Fixes cross-project memory bleed and the cold-start experience, plus Windows
install support.

### Added

- **Cold-start auto-bootstrap** — the first time a project is seen (empty
  store), the SessionStart hook seeds its memory from repo signal (README,
  docs, `CLAUDE`/`AGENTS`/`GEMINI.md`, tree) by spawning `anchored bootstrap`
  as a detached child, so a fresh project isn't a silent, empty store. Opt out
  with `plugin.auto_bootstrap: false`.

### Fixed

- **Cross-project preference leak** — a new project's `<identity>` block
  surfaced preferences captured while working in unrelated projects.
  `synthesizeIdentity` now scopes to user-level (no project) plus the current
  project, and drops hard curation rejections.
- **Categorizer false positives** — bare `nunca`/`never` and `padrão é`/`regra
  é` no longer miscategorize ordinary sentences ("senha nunca exposta") as
  preferences; the patterns require a usage verb / personal possessive.
- **Windows installer** — `install.sh` now supports Git Bash / MSYS / Cygwin
  (`MINGW*`/`MSYS*`/`CYGWIN*`), extracting the published Windows `.zip`
  (`anchored.exe`) instead of aborting with "Unsupported OS".

## [0.14.0] - 2026-07-19

Broadens which agent hosts anchored plugs into, and surfaces more of the memory
layer in the local dashboard.

### Added

- **More hosts in `anchored init --tool`** — registers the anchored MCP server
  into **OpenClaw** (`~/.openclaw/openclaw.json`), **Hermes**
  (`~/.hermes/config.yaml`), and the devclaw-derived agents **devclaw** and
  **gatorclaw** (`~/.<host>/config.yaml`, a `servers[]` array under `mcp`). New
  YAML map/array writers preserve foreign keys and are idempotent.
- **Deep host plugins** — for hosts with a memory slot, `init` also installs a
  plugin that bridges the agent lifecycle to the local anchored binary (recall
  via `anchored search`, capture via `anchored save`), no REST daemon:
  OpenClaw (`.mjs`), Hermes (Python), and **pi** (a TS extension, since pi has
  no MCP surface).
- **Dashboard: tokens-saved card** — the Overview shows context tokens injected
  vs. the static-context baseline over the last 7 days, from the v0.13
  `recall_logs` telemetry (`GET /api/tokens`).
- **Dashboard: Conexões tab** — lists every host anchored can register into and
  whether it's installed / registered, with the exact `anchored init --tool`
  hint (`GET /api/connections`).

### Changed

- **Dashboard polish** — wide tables scroll inside their own panel instead of
  pushing the page sideways, the body never scrolls horizontally, and card
  grids collapse to a single column on small screens.

## [0.13.0] - 2026-07-19

Context-quality release: the agent's work is now seen and the memory layer's
value is measured. Identity fills itself from what you've taught anchored,
duplicate tool events stop cluttering the timeline, retrieval stops letting one
session dominate, budgeting counts tokens instead of bytes, and a new telemetry
surface shows how many context tokens anchored saves.

### Added

- **Identity synthesized from preferences** — when `~/.anchored/identity.md` is
  still the install template, `anchored_context` now fills the `<identity>`
  block from your top preference memories instead of shipping an empty
  skeleton. A curated identity file keeps full precedence; `anchored doctor`
  reports a template-only file instead of calling it healthy.
- **Context-token telemetry** — every context injection records tokens injected
  vs. the static-context baseline (CLAUDE.md/AGENTS.md + skills) it stands in
  for, in a new `recall_logs` table (baseline cached per project, recomputed at
  most daily). `anchored stats --tokens` reports injected vs. baseline and
  savings % over the last 7 days.

### Changed

- **Retrieval diversifies by origin** — hybrid search now caps how many results
  come from the same source session (default 3), so one prolix session can't
  dominate the top-k. Set `DiversifyPerOrigin` to 0 to disable.
- **Context budget measured in tokens** — the SessionStart rich block is now
  budgeted in approximate tokens (default 2000, `sessionstart_budget_tokens`)
  rather than bytes, matching what actually fills the model's context window.
  Legacy `sessionstart_budget_bytes` configs are converted automatically.

### Fixed

- **Duplicate PostToolUse events** — a byte-identical tool call fired again
  within 5 minutes (same session + tool + summary) is no longer recorded twice,
  keeping the session timeline clean. Fail-open: a genuine event is never
  dropped.

## [0.12.0] - 2026-07-15

Task management lands across every surface — dashboard, MCP tool, and CLI now
share one cross-project task-thread store — alongside a dashboard visual
refresh and a fix that stops orphaned stdio servers from leaking memory.

### Added

- **Task management in the dashboard** — a new Tasks tab with a Kanban board
  (active / paused / done / cancelled), native drag-and-drop status changes,
  and a create/detail modal with a journal timeline. Surfaces the existing
  cross-project `task_threads` store; manual creation coexists with
  agent/CLI-created threads via same-key upsert.
- **`anchored_task` MCP tool** — lets agents manage task threads during a
  session (`start` / `note` / `status` / `list` / `get`), linking the current
  session and project on every action. A routing-block guardrail keeps the
  agent from marking a task done/cancelled without explicit user intent, and a
  task is never closed just because a conversation ends.
- **Task API** (`/api/tasks`) on the dashboard — list (with status filter),
  create, get, patch (status + journal/ref/project), and soft-cancel, behind
  the existing DNS-rebinding / CSRF / token guard.
- **`anchored bootstrap --skip-embeddings`** and an automatic embedding
  backfill after bootstrap, so a freshly seeded store is searchable without a
  separate step (#4).
- **`make sync-bin` / `sync-bin-dry`** — refresh every installed `anchored`
  binary with a freshly built one (#4).
- **VS Code Copilot** setup docs (MCP + hooks) in the README (#4).

### Changed

- **Dashboard visual redesign** — dark-first layout with a left sidebar
  (replacing the top tabs), a cyan accent, and monospaced data. All existing
  views are preserved.

### Fixed

- **Orphaned stdio servers now terminate on client exit.** The MCP server only
  stopped on stdin EOF, so a crashed or lingering client left it running and
  pinning the ~450MB ONNX model in RAM. It now shuts down on SIGINT/SIGTERM,
  polls for parent-process death (with `PR_SET_PDEATHSIG` on Linux), and has a
  forced-exit backstop — all preserving the existing EOF path. Opt out of the
  parent watchdog with `ANCHORED_NO_PARENT_WATCH=1`.
- **ONNX model detection** now handles the nested `<dir>/<model>/model.onnx`
  layout in `doctor` and `init` via a shared `findONNXModel`, fixing a false
  "model not found" in `init` and a cross-directory mismatch in `doctor` (#4).

## [0.11.0] - 2026-07-06

Real Cursor activation and a fix for the embedding backfill that had left the
historical store almost entirely un-embedded.

### Added

- **Real Cursor support** (`anchored init --tool cursor`) — beyond MCP
  registration, init now installs `~/.cursor/hooks.json` (wired to Cursor's
  actual hook schema: `beforeShellExecution`, `beforeMCPExecution`,
  `afterFileEdit`, `beforeSubmitPrompt`, `stop`) and an always-on activation
  rule at `~/.cursor/rules/anchored.mdc`. The rule is the agent-side trigger
  Cursor needs because its `beforeSubmitPrompt` hook is informational-only and
  cannot inject recall context. hooks.json is merged non-destructively —
  foreign entries are preserved and a bare `anchored` command is repaired to
  an absolute path. A `configs/cursor/probe-hooks.json` template is included
  for capturing real Cursor payloads.
- **Cursor hook routing** — a payload adapter (`pkg/hookroute`) detects
  Cursor's envelope and routes its events through the existing hook pipeline,
  emitting Cursor's permission wire shape (`{continue, permission, agentMessage}`).
  The Claude Code path is unchanged. The dangerous-pattern guard for the
  sandbox tools applies to Cursor's `beforeMCPExecution` as well.
- **`anchored backfill --max N`** — bounds embeddings per run so a large
  backlog drains in slices; wired into `maintenance run` as a step so the
  daily timer chews through it without an agent connected.
- **New doctor checks** — Cursor hooks/rule installation, embedding coverage
  (warns below 80%), maintenance timer status, and debug-log staleness.

### Fixed

- **Tokenizer O(n²) stall on giant words** — a single very long unbroken token
  (e.g. a minified/base64 blob in an imported memory) could hang the ONNX
  tokenizer indefinitely at full CPU, which stalled `anchored backfill` and
  left the store at ~6% embedding coverage. Input is now bounded before
  tokenization (rune-safe), so the backlog embeds normally.
- **PATH-dependent MCP registration** — `init` wrote a bare `anchored`
  command that only resolved via `PATH`, breaking when a tool launched its
  MCP server outside a login shell. It now writes the resolved absolute
  binary path (and repairs existing bare entries in place across all tools).
  Plugin hook commands are rewritten to the absolute path on install too.
- **`anchored doctor` printed `vv0.10.0`** — the version string already
  carried its `v` prefix; the double-`v` is gone.
- **Stale embedding-model label** in the shipped `config.yaml` template now
  matches the model bootstrap actually downloads
  (`paraphrase-multilingual-MiniLM-L12-v2`).

## [0.10.0] - 2026-06-30

Two contributions from @aronpc: a local web dashboard (PR #2) and a
security fix + maintenance timer + MCP test coverage (PR #3).

### Added

- **Local web dashboard** (`anchored dashboard`) — browse stats, search, the
  knowledge graph, and store health from a loopback-bound web UI; `dashboard
  install` registers it as a systemd `--user` service. Hardened for local-only
  use (loopback bind, Host guard, CSRF/Origin checks, CSP, optional `--token`
  that gates reads and writes).
- **Periodic maintenance** (`anchored maintenance`) — `run` chains import +
  dream + curation as isolated subprocesses (a failure in one step does not
  abort the others); `install` wires it into a daily systemd `--user` timer
  (persistent, jittered) so upkeep happens without an agent connected.

### Fixed

- **crypto/rand errors are no longer ignored** in the id generators
  (`newUUID`, `newID`): a silent failure would have produced non-unique
  all-zero ids. They now panic, the standard treatment for an unrecoverable
  CSPRNG failure.

### Tests

- Added MCP unit coverage (`SetDebugLogger`, `headTail`, `MarshalResponse`
  error path, `searchHitWriter`) and maintenance unit/orchestration tests.

## [0.9.1] - 2026-06-21

Stops the agent from abandoning anchored when its tools are deferred in the
harness — the follow-up to v0.9.0's reliable-invocation work.

### Fixed

- **Sandbox redirects guide deferred-tool harnesses** — the optimizer redirects
  (curl/wget, WebFetch, inline HTTP, build tools) point the agent at
  `anchored_execute` / `anchored_fetch_and_index`. When those tools are deferred
  (Claude Code loads anchored_* schemas on demand), a direct call fails as
  not-found and the agent abandoned anchored entirely, falling back to nothing
  since the original command was blocked. Each redirect now tells the agent to
  `ToolSearch`-load the tools first and not to drop the task on a not-found.
- **Deferred-tool bootstrap in the main routing block** — the `ToolSearch`
  bootstrap previously lived only in the subagent block. It is now in
  `AnchoredRoutingBlock` (SessionStart) and, compactly, in
  `AnchoredMCPInstructions`, so the main agent learns once to load the anchored
  tools — covering every call site (gate, redirects, secret-block, miss-nudge)
  systemically. Mirrors context-mode's fix for the same stall (their #724).

## [0.9.0] - 2026-06-20

Makes anchored reliably invoked on every session and prompt — especially in
Claude Code, which routinely ignored the soft memory-routing injection when
handed a concrete task. The context gate becomes the floor behavior, the plugin
self-heals a wedged marketplace mirror, the MCP instructions survive client
truncation, and the recall push gains real vector recall.

### Changed

- **Context gate is ON by default** — `context_gate` now resolves to `enforce`
  for every value except an explicit `context_gate: disabled` (the only opt-out).
  The legacy `off` the previous default serialized into existing configs also
  resolves to `enforce`, so a version bump alone arms the gate with no config
  edit. It still fires at most once per session and relents after a few denies,
  so it can never hard-block work.
- **Auto-recall defaults to `full`**, and `Defaults()` now enables MMR and
  temporal decay, so fresh installs get ranked, deduplicated, freshness-aware
  results out of the box instead of raw BM25 order.

### Added

- **Vector recall in the UserPromptSubmit push** — when keyword (BM25) recall is
  sparse, anchored seeds from the top hit's precomputed embedding and pulls its
  nearest neighbours from the vector cache (memory-to-memory expansion, no
  raw-prompt embedding), bounded by a 100ms budget and fail-open. When keyword
  recall finds nothing, it injects a directed nudge to call `anchored_search`
  (the serve-side hybrid search) instead of the generic reminder.
- **PostToolUse context-gate fallback** — the gate is credited even when the
  PreToolUse credit is missed (e.g. a stale plugin matcher), so consulting
  memory always satisfies it.

### Fixed

- **Self-heal a stale marketplace mirror** — a dirty or hand-edited mirror made
  `git pull --ff-only` fail and permanently pinned the plugin to a stale
  version. Anchored now hard-resets the managed mirror to its upstream and
  retries within the same budget, so a binary update heals the plugin install
  instead of wedging on a leftover local edit.
- **MCP server instructions survive Claude Code's 2048-char truncation** — the
  handshake now ships a separate compact `AnchoredMCPInstructions` constant
  (~1.6KB) that keeps every load-bearing directive; the full rich block still
  ships verbatim via the (untruncated) SessionStart hook.
- **Gate deny guides deferred-tool harnesses** — the deny reason now tells the
  agent to `ToolSearch`-load the anchored tools first when they are deferred,
  collapsing repeated denies to a single one.
- **Plugin manifest versions** (`plugin.json`, `marketplace.json`) now track
  `VERSION`, so drift detection no longer re-syncs the plugin every session.

## [0.8.9] - 2026-06-15

Quiets the MCP server's log output and bounds every tool response that scales
with stored or executed content, so neither floods the host (Cursor's error
pane, or Claude Code's on-disk persistence of oversized results).

### Fixed

- **Server no longer spams MCP clients with false `[error]` lines** — the
  `serve` logger now defaults to WARN instead of INFO. STDIO MCP clients (e.g.
  Cursor) surface every stderr line as an `[error]`, so routine operational
  INFO (`vector cache loaded`, `ONNX embedder initialized`, and the recurring
  eviction-cycle logs that fire every few minutes) was showing up as a steady
  stream of fake errors in the client's log pane. Warnings and errors still
  pass through; set `ANCHORED_LOG_LEVEL=debug|info|warn|error` to override when
  diagnosing the server itself.
- **Tool responses are now bounded** — `anchored_search`, `anchored_list`, and
  `anchored_execute`/`execute_file`/`batch_execute` cap their output (per-hit
  rune cap + whole-response byte budget; head/tail previews for large execution
  output and echoed stderr). An oversized result used to be persisted to a file
  by Claude Code, defeating the point of the plugin; over-budget search hits are
  now counted in an `<omitted>` note instead of silently dropped.
- **Team memory no longer silently skipped on an omitted `cwd`** — search, save,
  and `kg_add` resolve an empty `cwd` to the server's working directory (the
  same convention `save` already used), so the auto remote merge can find the
  linked project instead of degrading to local-only. An unresolved-but-
  configured remote now leaves a debug trace.
- **Hook matchers corrected** — `mcp__` → `mcp__.*` and the `PostToolUse`
  matcher split so `Write|Edit|Bash` and `mcp__anchored__.*` are matched
  independently.

### Added

- **`anchored backfill`** — standalone, observable one-shot that embeds every
  memory still missing a vector (`--batch`, `--pause` to stay gentle on CPU).
  Idempotent and resumable; does not depend on the MCP server being up.
- **`anchored_search` `full=true`** — lifts the per-hit preview cap for reading
  one specific memory in full (pair with a low `max_results`).
- **`context_gate` plugin option** (off by default) — optional deterministic
  PreToolUse enforcement that the agent consult anchored memory before its first
  substantive tool call in a session. Bounded and fail-open: relents after a few
  denies and never hard-blocks the user.

## [0.8.8] - 2026-06-11

Observability pass over the MCP tools and hooks: failures that used to vanish
silently now leave a trace, making "why didn't anchored remember/find X?"
diagnosable.

### Fixed

- **MCP server no longer swallows errors** — `anchored_context` now logs
  failures from session-activity recording, recent-memory listing, and KG
  triple listing; `anchored_execute` logs when auto-indexing a large output
  fails; a skipped remote merge during `anchored_search` is logged at warn
  (it silently degraded team search to local-only). All paths keep their
  best-effort behavior — nothing new fails, it just stops failing invisibly.
- **SessionStart hook logs parse/query failures** — malformed stdin JSON and
  working-set lookups that error are recorded in the debug log instead of
  being discarded; the hook remains fail-safe.
- Removed dead `_ = p.CWD` placeholders in `anchored_forget`/`anchored_update`.

## [0.8.7] - 2026-06-11

Hotfix for v0.8.6: the new PreToolUse routing emitted output Claude Code's hook
schema rejects, spamming `PreToolUse:Read hook error — (root): Invalid input`
on nearly every tool call.

### Fixed

- **No more `additionalContext` from PreToolUse** — Claude Code's PreToolUse
  hook schema accepts only `permissionDecision`, `permissionDecisionReason`, and
  `updatedInput`; there is no `additionalContext` channel (that field exists
  only for SessionStart / UserPromptSubmit / PostToolUse). The soft "search
  memory first" nudges on `Read`/`Grep`/`Glob`/generic `Bash`/external MCP had
  no valid wire shape and failed validation, so they are removed. That messaging
  already lives in the SessionStart block, the per-prompt UserPromptSubmit
  reminder, and the skill — all of which DO support `additionalContext`. The
  high-value enforcement is unchanged: `WebFetch`/`curl`/`wget`/inline-HTTP/
  build-tool **deny + redirect**, and `Agent`/`Task` subagent prompt injection.
- **Passthrough no longer emits `{"decision":"allow"}`** — Claude Code types the
  top-level `decision` field as `enum("approve","block")`, so `"allow"` is an
  invalid value that failed validation on every passthrough. The hook now emits
  an empty object `{}` (a valid no-op) instead. This was a latent bug that only
  surfaced once v0.8.6 widened the PreToolUse matchers to `Read`/`Grep`/`Bash`.

## [0.8.6] - 2026-06-11

The model now reaches for memory before it reaches for files. Anchored gains a
PreToolUse routing layer — the same enforcement mechanism context-mode uses —
so native exploration tools are steered toward Anchored's memory and sandbox
tools instead of being silently bypassed.

### Added

- **PreToolUse routing (`pkg/hookroute`)** — `Read`/`Grep`/`Glob`/`Bash`/
  `WebFetch`/`Agent` and external MCP calls are now intercepted at the point of
  action and steered toward Anchored:
  - **Memory-first nudges** on codebase search (recursive `grep`/`rg`/`ag`/
    `find`, `Grep`, `Glob`): "search memory before exploring". Re-fired
    periodically so they survive context compaction.
  - **WebFetch → deny + redirect** to `anchored_fetch_and_index` +
    `anchored_ctx_search` (raw page bytes stay out of context).
  - **`curl`/`wget` stdout floods, inline HTTP in `-e`/`-c` snippets, and
    verbose build tools (gradle/maven/sbt) → deny + redirect** to
    `anchored_execute`. Silent file downloads, quoted text, and structurally
    bounded probes (`pwd`, `git status`, `--version`, …) pass through untouched.
  - **Subagent prompt injection** (`Agent`/`Task`) — subagents now receive a
    compact memory routing block plus a `ToolSearch` bootstrap for the deferred
    `anchored_*` tools, so work done in subagents uses memory too.
  - All sandbox redirects are gated on `context_optimizer.enabled` — when the
    optimizer is off they degrade to a soft nudge instead of denying into a
    tool that would error.
- **Skill** — `skills/anchored/SKILL.md` gains a mandatory "memory-first" rule,
  a decision tree, and an anti-patterns section.

### Changed

- **`context_optimizer.enabled` now defaults to `true`** — the sandbox tools
  (`anchored_execute*`, `_fetch_and_index`, `_ctx_search`) and the routing
  redirects that target them only work when the optimizer is on. (Existing
  configs that set it explicitly are unchanged; flip it manually to opt in.)
- **PreToolUse hook output migrated to `hookSpecificOutput`** — security blocks
  (secret-into-memory-file, dangerous sandbox patterns) now emit
  `permissionDecision: "deny"`, the shape Claude Code reliably honors.

### Fixed

- **Dangerous-pattern check now matches the fully-qualified MCP tool name** —
  it compared `anchored_execute` but Claude Code sends
  `mcp__anchored__anchored_execute`; the check now strips to the leaf name so
  the sandbox security gate actually fires.

## [0.8.5] - 2026-06-10

### Added

- **Personal kanban sync** — every `anchored task` mutation
  (start/pause/resume/done/cancel/note) mirrors the thread to your server as
  a card for the dashboard's new "My tasks" board (server v0.5.6+). Best
  effort by design: no remote, no key or a network failure never fails the
  local command. **Privacy gate:** the journal (free-form personal notes)
  is only ever sent to your DEFAULT (personal) server — when cwd routing
  resolves to a team server, the card syncs as metadata only
  (key/status/projects) and the command says so.

## [0.8.4] - 2026-06-10

The store stops growing forever: related memories consolidate into summaries
and never-used ones fade with age.

### Added

- **Cluster synthesis (`anchored dream`)** — connected components of >= 3
  near-duplicate memories become a "synthesize" proposal. Applying it creates
  one deterministic summary memory (member IDs recorded in
  `metadata.consolidated`/`supersedes`) inside a transaction, and DEMOTES the
  raw members (`low_signal/consolidated`, with `consolidated_into`) — never
  deletes them. Consolidated members are exempt from the mechanical
  recuration lift: their demotion is structural, undone only explicitly.
- **Age decay at search time** — memories never drawn on fade gently with
  age (x0.85 after 90 days unused, x0.7 after 180), computed during ranking
  with nothing written back. Any recorded use resets the clock; pinned
  memories never decay; decay never stacks on an already-demoted memory
  (the stacked-multiplier lesson from 0.5.8). Explain mode reports a
  `decayed` signal.

## [0.8.3] - 2026-06-10

Cross-project task threads: a ticket becomes a first-class unit of work that
follows you across repositories.

### Added

- **Task threads (`anchored task`)** — `start/pause/resume/done/cancel/note/
  status` manage a ticket-keyed thread (PROJ-123) that records every project
  and session the task touches plus a short journal. `done` consolidates the
  thread into a durable summary memory (project names, ref, journal recap);
  `cancel` closes without consolidating. No interactive prompts — every
  transition is a command. Local-only (no sync in v1); migration
  `016_task_threads` is additive and idempotent.
- **Branch inference** — the SessionStart hook extracts the ticket key from
  the checked-out branch (`feature/PROJ-123-...`, case-insensitive,
  `git symbolic-ref` so branches with no commits still resolve) and registers
  the session on the thread automatically. Done/cancelled threads are never
  silently reopened by automation; paused threads reactivate on touch.
- **Cross-repo context injection** — when the same task already touched OTHER
  projects, the rich SessionStart block gains a compact `<task_thread>` tier:
  thread status, latest journal notes, and per-project `<also_touched>` lines
  with the files those sessions worked on. References only — memories keep
  their own project; the thread just ties the strands together.

## [0.8.2] - 2026-06-10

The usage feedback loop closes: memories that keep getting injected but never
help are pulled out of rotation automatically, and a single real use brings
them back.

### Added

- **Used-signal capture** — the Stop hook now checks which injected memories
  the turn's text actually drew on (deterministic significant-token overlap,
  no model in the loop) and bumps `used_count`/`last_used_at`. Runs even when
  the turn produced nothing to auto-save.
- **Never-used demotion** — the canonical recuration pass demotes memories
  with `injected_count >= 10` and `used_count == 0` to
  `low_signal/never_used`. Advisory and reversible: one real use lifts the
  flag on the next pass. Pinned memories are exempt, and the mechanical
  quality rule takes priority. `curation_rule` metadata explains which rule
  set the flag.
- **Push-path demotion** — the auto-recall hook now excludes `low_signal`
  memories, so a demotion actually stops the injection (previously only
  `Service.Search` honored it).

- **Trustworthy explicit-remote search** — when `anchored_search` is asked
  for a remote exclusively and that search fails, the local results now come
  back marked with `remote_error="..."` and `fallback="local"` instead of
  silently impersonating the remote (which led agents to conclude "local and
  remote are in sync" when the remote was never reached). And the
  `""`/`"default"` selector now means THIS REPO'S remote — resolved by the
  same git-origin routing sync uses — not whichever config entry happens to
  be named "default".

### Changed

- Quality scorer version bumped to 3 — the serve-time worker and
  `curation reconcile` re-flow the whole corpus once. As part of that pass,
  `injected_count` accumulated on v0.8.x (before the used-signal existed) is
  reset, so no memory is demoted for injections it never had a chance to be
  marked "used" against.

### Fixed

- `curation reconcile` no longer aborts on memories with a NULL `source`
  column (rows created by raw tooling or older imports).

## [0.8.1] - 2026-06-09

### Added

- **User directives (standing rules)** — `anchored directives add|list|rm`
  manages short do/don't rules ("never commit without an explicit request").
  They are first-party instructions, stored as pinned preference memories
  marked `directive=true`, and the SessionStart hook injects them as the
  **top tier** of the rich context block — always present, unranked, ahead of
  identity/decisions — rendered as `<standing_rule scope="user|project">`
  lines. Global rules load in every project; `--project` binds a rule to the
  current repo. `rm` accepts a unique ID prefix and only matches
  directive-marked rows.
- **Injection tracking (usage-feedback capture)** — when auto-recall injects
  memories, their IDs are merged into the session working set
  (`working_sets.memory_ids`) and each memory's metadata gets
  `injected_count`/`last_injected_at` bumped, on a best-effort 50ms write that
  can never delay or fail the prompt. This is the data-collection half of the
  usage feedback loop: a future curation pass can demote
  always-injected-never-used memories, and "injected × used" becomes
  measurable.

### Fixed

- `anchored directives rm` accepts flags after the positional ID
  (`reorderArgsForFlag`), matching the other subcommands.

## [0.8.0] - 2026-06-09

Push-based context injection: the hooks now put relevant memory in front of the
model (and capture durable knowledge) instead of relying on the model calling
the memory tools.

> **Note:** after the plugin updates, restart Claude Code so the new `Stop`
> hook in `hooks.json` is registered.

### Added

- **`pkg/contextbudget`** — deterministic tiered context assembler with a byte
  ceiling: tiers fill in priority order, items are dropped whole (never split),
  a higher tier never loses an item to a lower one, and `MinItems` reserves a
  minimum per tier.
- **Rich SessionStart context block** — the SessionStart hook now assembles
  identity, pinned + recent decisions/learnings, the session working set, and
  recent events into a budgeted `<anchored_context>` block
  (`plugin.sessionstart_budget_bytes`, default 7000; `0` restores the previous
  plain format).
- **Stop hook (`anchored hook stop`)** — extracts durable decision/learning
  candidates from the turn transcript (PT+EN markers, paragraph fallback for
  short decisive sentences) and auto-saves them with dedup (content hash +
  token Jaccard against the 50 most recent memories), capped at 2 saves per
  turn with a 30-day TTL. Saves are embedder-free (`embedding NULL`; the
  curation worker embeds asynchronously) so the hook stays well under its
  500ms budget. Loop-guarded via `stop_hook_active`; disable with
  `plugin.auto_save_stop: false`.
- **Recall v2 in UserPromptSubmit** — the auto-recall query is now anchored to
  the code being discussed: file/symbol anchors extracted from the prompt,
  session working-set signals (files/symbols/entities), an expanded BM25 query
  where anchors take precedence (24-token cap), a local re-rank boost with
  explainability signals (`file_anchor`, `working_set`), and an optional
  compact `<anchored_kg>` line when an anchor matches a knowledge-graph
  entity.
- **Adaptive recall reminder** — a strong one-line nudge when boosted hits are
  injected ("consult before exploring files"), a short variant when recall is
  empty (`plugin.adaptive_reminder`).
- **Eval fixture** — recall fixture now includes a file-path-anchored query.

### Changed

- BM25 recall candidate limit raised from 3 to 5; the existing hook byte
  budget decides how many survive.
- `hooks/hooks.json` registers the new `Stop` hook.

## [0.5.8] - 2026-05-28

### Added

- **Versioned quality scorer** — memories now carry a `scorer_version` in metadata. When the scoring formula changes, the worker and the new reconcile command re-flow scores through the whole corpus instead of only touching brand-new memories.
- **Automatic backlog bootstrap** — on `anchored serve` startup, the curation worker performs a one-time full re-curation of the existing corpus when `scorer_version` lags (guarded by `reconciled_version` in `curation_state`), then settles into incremental passes. Repairs legacy memories with no manual command required.
- **`anchored curation reconcile`** — re-scores the entire corpus in a single pass and repairs stale `low_signal` flags; records `reconciled_version` so the serve bootstrap does not redundantly re-drain.
- **Richer `curation status`** — now shows the scorer version, corpus reconcile state, and pending candidate count.

### Changed

- **Curation worker selects by `scorer_version`** (missing or below current) rather than only unscored memories, so an existing corpus is actually re-curated after a scorer change.
- **Single canonical scoring path** — `RecurateMetadata` is now shared by the save path, the worker, and the CLI, replacing three divergent implementations with different `low_signal` clear thresholds.
- **`importance` is only initialized, never reduced** — the worker no longer ratchets a deliberately-set importance down toward the mechanical quality score.
- **MCP routing guidance** — the server instructions and `anchored_save` description now assert anchored as the authoritative memory store and treat an explicit "remember/save" request as an unconditional trigger, so agents stop routing memory to native features or local files.

### Fixed

- **Search demotion no longer stacks** — `low_signal` (×0.03) and the sub-threshold quality band (×0.15) were multiplied together (~0.0045), effectively erasing legitimate hits. They are now mutually exclusive, which unburies the large share of the corpus that older formula versions had over-flagged.
- **Embeddings persist on save** — the create path assigned the memory ID to a by-value copy, so `embedAsync`/observers ran with an empty ID and the embedding column was never populated. The ID is now assigned before save.

## [0.5.7] - 2026-05-28

### Added

- **Default-on curation worker** — `anchored serve` now starts a safe background curation worker by default. It runs in small incremental passes, processes newest memories first (`updated_at DESC, created_at DESC`), and only refreshes lifecycle metadata (`quality_score`, `importance`, `curation_status`). It never rewrites content, soft-deletes, or hard-deletes memories automatically.
- **Curation worker controls** — new `anchored curation status`, `anchored curation enable`, and `anchored curation disable` commands. Status is lightweight and reads config/state without booting the full memory service or ONNX/FTS stack.
- **Curation config block** — new `curation.*` settings: `enabled`, `interval_hours`, `interval_minutes`, `threshold`, and `max_updates_per_run`. `interval_minutes` overrides `interval_hours` when set and enables short-cycle local maintenance.
- **Curation state table** — migration `012_curation_state` stores worker state such as `last_run_at` so serve-time curation is resumable and does not depend on a long-lived daemon.

### Changed

- Default curation policy is now short and incremental: enabled, `interval_minutes=15`, `threshold=0.55`, and `max_updates_per_run=50`.
- Curation now distinguishes the always-on safe path from `dream`: curation handles cheap metadata health; dream remains explicit/manual for deduplication, merge/supersede actions, and contradiction review.
- README rewritten for clearer onboarding, CLI organization, curation/dream explanation, MCP tools, storage, configuration, and development/release flow.

### Verified

- Built a release test binary and ran an end-to-end curation worker pass against an isolated copy of the local database with `interval_minutes=1` and `max_updates_per_run=5`. Logs confirmed two one-minute passes, each scanning/updating 5 memories, and `curation_state.last_run_at` was written.
- `go test ./pkg/config ./pkg/kg` passes.
- `go test ./cmd/anchored ./pkg/memory ./pkg/mcp -run '^$'` compiles FTS-dependent packages in this environment.

## [0.5.5] - 2026-05-25

### Added

- **Curation pipeline** — `anchored curation score [--apply] [--threshold 0.55]` computes a heuristic quality score per memory (length, category, signal patterns, project association) and marks low-signal ones with `metadata.curation_status=low_signal`. Pinned memories are always exempt. Dry-run by default; `--apply` persists.
- **Curation clean** — `anchored curation clean [--hard] [--threshold] [--dry-run] [--yes]` removes low-signal memories. Soft-delete by default (reversible); `--hard` for permanent deletion. Prints a sample of the lowest-quality candidates before applying.
- **Curation restore** — `anchored curation restore [--latest | --from PATH]` swaps the active DB for a backup from `~/.anchored/data/bkps/`. The current DB is snapshotted before the swap (`anchored.db.bak.pre-restore-<ts>`), so the operation is reversible. Interactive picker when no flag is given.
- **Per-project remote sync** — `anchored remote sync-per-project [--min-memories N]` groups local memories by `project_id`, creates one remote project per local project, and pushes each subset separately. Preserves project segmentation on `anchored_oss` (devclaw, gatorllm, etc stay distinct on the team server instead of collapsing into one bucket).
- **Knowledge-graph push** — `sync.Client.PushTriples(projectID, triples)` sends to `POST /v1/projects/{id}/triples` on the OSS server. `kg.KG.ListByProject(projectID)` enumerates live triples for a project. Server-side: idempotent (logical unique on subject+predicate+object+project), supports functional supersession, alias resolution.
- **Hybrid-search lifecycle demotion** — `applyLifecycleBoost` multiplies score by 0.03 for `curation_status=low_signal` and by 0.15 for `quality_score < RemoteQualityThreshold` (non-pinned). Effectively buries junk from search results without deleting.
- **Quality-aware sync filter** — `IsRemoteSyncCandidate` and `ClassifyForPreview` block `low_signal` and below-threshold `quality_score` memories. Preview surfaces them as `low_signal` / `low_quality` reasons distinct from the legacy `category_remote_blocked`.
- **Hardened secret detection** — `pkg/sync.detectSecrets` adds explicit prefix/regex matchers for Stripe (`sk_live_`, `sk_test_`, `rk_live_`), GitHub (`ghp_`, `gho_`, `ghu_`, `ghs_`), Slack (`xoxb-`, `xoxp-`, `hooks.slack.com/services/T…`), AWS access keys (`AKIA[0-9A-Z]{16}`), Google API keys (`AIza[0-9A-Za-z_\-]{35}`), credentialed DB URIs (mongo/postgres/mysql/redis with `user:pass@`), and PEM private keys.
- **Lifecycle-aware metadata propagation** — `SyncMemory.Metadata` is now sent over the wire; the remote server applies its own quality/lifecycle filter as defense-in-depth.

### Changed

- `RemoteQualityThreshold = 0.55` is now an exported constant shared by client (preview, sync, hybrid-search demotion) and consumed by `anchored_oss` server filter via metadata.
- `Service.SaveWithOptions` and `Service.Update` automatically apply `ApplyQualityMetadata` so every save/update gets a `quality_score`, `importance`, and (if low) `curation_status`. Previously these were only set by explicit curation runs.
- `SyncPushRequest.Memories[i].Metadata` is now `any` and propagated end-to-end. The remote can see lifecycle/curation flags it needs to enforce its own filter.
- `SQLiteStore.UpdateMetadata` added: targeted metadata-only update (does not bump `updated_at`, does not re-index FTS). Used by `curation score --apply` to avoid hammering FTS over 25k rows.
- `memories_fts_update` trigger now fires only on `UPDATE OF content, keywords` — metadata-only writes no longer rebuild the FTS row.

### Fixed

- Duplicate `switch` block removed from `SQLiteStore.List` (prior accidental copy created at refactor time; behavior unchanged).
- `Push` short-circuits `event` and `preference` categories before the path/secret pipeline, matching the documented filter precedence and avoiding pointless sanitizer work.

### Internal

- New CLI files: `cmd/anchored/curation.go`, `cmd/anchored/curation_clean.go`, `cmd/anchored/remote_per_project.go`.
- New driver dep: `modernc.org/sqlite` (used by `remote sync-per-project` to read local project names directly without going through the embedded service).
- Pre-existing `pkg/memory` test suite still depends on FTS5 in the SQLite driver; those tests remain failing as in v0.5.0 (unrelated to this release; tracked separately).

## [0.5.0] - 2026-05-24

### Added

- **Memory lifecycle v2** — new `MetadataV2` struct with Kind, Scope, MemoryType, Origin, Importance, ContextTier, Pinned, ExpiresAt, Supersedes, Consolidates, Confidence, ContentHash, DreamSource fields. Constructor helpers for semantic, operational, handoff, precompact, bootstrap, and dream metadata. `ParseMetadata`/`ParseMetadataFromJSON` with `Extra` passthrough for unknown keys. `IsExpired`, `IsSemantic`, `IsOperational`, `IsRemoteSyncCandidate` classifiers.
- **Hybrid search lifecycle boost** — `applyLifecycleBoost` runs before temporal decay so pinned/important items are not penalized before boost. Handles pinned (+1.5×), importance, kind (decision/learning/rule +1.15×, active handoff +1.2×, active precompact +1.1×), semantic (+1.1×), operational expiry, superseded (-0.7×), bootstrap confidence, and context tier (L0 +1.3×, L1 +1.15×). Score clamped at 10.0.
- **Dream supersede/merge actions** — supersede appends related_memory_id to metadata.supersedes; merge appends to metadata.consolidates and soft-deletes the merged memory. JSON errors are now propagated instead of silently discarded.
- **Lifecycle-aware sync preview** — `ClassifyForPreview` checks v2 metadata: episodic, precompact, handoff, user-scope, and operational memories are blocked from sync. Semantic project-scoped and dream-derived semantic memories are syncable.
- **CLI lifecycle commands** — `anchored bootstrap` (extract seeds from README/docs/rules/tree with SHA256 content hash and project-scoped dedup), `anchored handoff` (session handoff with scope and TTL, min 1h), `anchored retention sweep` (archive operational/episodic memories past TTL using proper time.Time comparison and UTC), `anchored precompact` (capture context snapshot with project-resolved scope).
- **SaveOptions.Metadata** — store and service accept lifecycle metadata through the save pipeline.
- **Tool support expanded** — `anchored init --tool` now supports 10 tools: Claude Code, Cursor, OpenCode, Gemini CLI, Antigravity (agy), Windsurf, Cline, VS Code Copilot, Codex CLI, and Devin. VS Code Copilot uses `servers` root key with required `type: stdio` field. Codex CLI uses TOML format (`~/.codex/config.toml`). Doctor probes added for all new tools with format-aware detection (JSON, TOML, VS Code `servers` key).
- **Antigravity 2.0 (agy) support** — `anchored init --tool agy` detects both `~/.gemini/config/mcp_config.json` (Antigravity 2.0 desktop) and `~/.gemini/antigravity-cli/mcp_config.json` (Antigravity CLI). macOS ONNX download path fix included.
- **Preference scope metadata** (Phase 1) — user/project/team scope metadata on preferences. `anchored save` accepts `--scope`.
- **Sanitizer custom patterns + remote safety filter** (Phase 2) — regex-based credential redaction with configurable custom patterns. Outbound sync content is scanned for local paths, secrets, and personal preferences.
- **Stable project identity with RemoteKey** (Phase 3) — git remote URL derived `RemoteKey` (SHA-256) for cross-machine consistency. Project identity survives directory renames.
- **Sync metadata migration + service observers** (Phase 4) — dirty flags, sync origin, author, remote project key columns and `sync_state` table. `MemoryObserver` interface for save/update/delete side effects.
- **Memory inspect and export CLI** (Phase 5) — `anchored inspect <id>` for full JSON details, `anchored export` with filters (JSON/JSONL, project, category, source, include-deleted).
- **Remote config and preview command** (Phase 6) — `anchored remote status` and `anchored remote preview` to classify memories as syncable/blocked/needs-review without network access.
- **Dream --apply single-action review** (Phase 7) — `anchored dream --apply <action-id>` for individual dream action review and application.
- **Minimal sync client skeleton** (Phase 8) — bidirectional sync client with push/pull/tombstone support.
- **Remote save and search** — `anchored save --remote` and `anchored search --remote` with graceful fallback when no remote is configured.
- **RemoteProjectKey and PreferenceScope** — Memory model extended with `RemoteProjectKey` and `PreferenceScope` fields for sync protocol compatibility.
- **Remote: paginated memory listing** — `remote` command paginates memory listing and extracts helpers for cleaner code.
- **Config wizard** — `anchored config wizard` for interactive configuration setup.
- **Contributing guide** — `CONTRIBUTING.md` with development setup, PR workflow, tool support guide, and coding conventions.
- **Lifecycle roadmap docs** — `docs/memory-lifecycle-roadmap.md` documents local lifecycle phases 1-8.

### Changed

- Go 1.25 (go.mod, ci.yml, release.yml).

### Fixed

- **Retention sweep** — `createdAt` parsed as `time.Time` with `.Before()` comparison instead of unreliable string comparison. UTC applied.
- **Precompact scope** — uses `svc.ResolveProject(cwd)` so scope is correctly `ScopeProject` when a project is detected (was hardcoded empty, always `ScopeUser`).
- **Consolidator JSON errors** — `json.Unmarshal` and `json.Marshal` in supersede/merge return explicit errors instead of silently discarding failures.
- **Bootstrap dedup** — project-scoped dedup query filters by `content_hash + project_id` instead of global content hash.
- **Hybrid search order** — `applyLifecycleBoost` now runs before `applyTemporalDecay` so pinned/important items are not decayed before boost is applied.
- **Handoff TTL validation** — `ttlHours` must be ≥ 1.
- **Sync: secret detection on path-rewritten content** — safety filter now scans content after project-relative path rewriting, catching secrets in rewritten paths.
- **Init: include gemini in --tool all** — `gemini` was missing from the `all` tool list.

## [0.4.10] - 2026-05-11

### Added

- **Zero-click plugin auto-install on SessionStart.** When the SessionStart hook detects `CacheBehind` (cache lags binary), anchored now copies the marketplace mirror tree into `<cache>/<newVersion>/` and rewrites Claude Code's `installed_plugins.json` so the new version is loaded on the next launch. The user only has to restart Claude Code — no `/plugin install` command needed. Combined with the existing `MirrorBehind` auto-sync, the full flow (binary update → mirror fast-forward → cache install → registry rewrite) runs without intervention.

### Safety

- **Registry schema gate.** `installed_plugins.json` rewrites are gated on `version: 2` (the schema we tested against). If Claude Code bumps the schema, anchored aborts the install with a clear notice and falls back to telling the user to run `/plugin install anchored@anchored` manually. Other plugins' entries are never touched.
- **Atomic file writes.** Cache install copies the mirror into a sibling `.tmp` dir then atomically renames; any pre-existing cache at the target version is backed up to `.bak` before the swap so a botched promotion can be undone. Registry rewrite goes through `tmp+rename`.
- **`installedAt` preserved.** The user's "first installed" timestamp is inherited from any existing registry entry; only `lastUpdated` and `gitCommitSha` are refreshed.
- **`.git` excluded from cache copy.** The marketplace mirror's git metadata stays in the mirror; the cache holds only the runtime files Claude Code needs.

### Changed

- **`<anchored_plugin_update>` notice gained `cache_installed="true"`.** When the full auto-install succeeded the notice now reads "Plugin auto-updated to vX (mirror + cache + registry). Restart Claude Code." Fallback paths (schema mismatch, git pull failure) still render their own copy.

## [0.4.9] - 2026-05-11

### Fixed

- **Plugin sync no longer corrupts Claude Code's plugin registry.** v0.4.7/v0.4.8 removed the cache directory as part of auto-sync, but `installed_plugins.json` is Claude Code's state and it keeps pointing at the deleted path — leaving the plugin in a "ghost install" state where Claude Code thinks v0.4.0 is installed at a non-existent directory and refuses to reinstall. `applyPluginAutoUpdate` now only fast-forwards the marketplace git mirror and never touches the cache. The user is told to run `/plugin install anchored@anchored` (idempotent) to pick up the new files.
- **Drift detection looks at the mirror too.** Previously the hook only compared `cache version` vs `binary version`; when the cache was absent or recently cleared, drift was reported as false and the mirror never caught up to new releases. `PluginDrift` now tracks both signals independently: `MirrorBehind` (mirror lags binary — anchored fixes via git pull) and `CacheBehind` (cache lags mirror/binary — user must run `/plugin install`). The notice text adapts to which signal(s) are set.

### Changed

- **Notice attributes renamed for clarity.** The `<anchored_plugin_update>` element now carries `binary="..." mirror="..." cache="..." [mirror_synced="true"]` instead of the older `installed=... binary=... [auto_synced=...]`. `cache="absent"` is rendered explicitly when no cache version is detected. The four notice paths (auto-synced, sync-failed, manual-fix, cache-only) each get tailored copy.

## [0.4.8] - 2026-05-11

### Fixed

- **Windows cross-compile** — v0.4.7 release artifacts never published because
  `syscall.Flock` and `syscall.LOCK_*` aren't defined on Windows, breaking the
  GoReleaser Windows target. `tryAcquireSyncLock` is now split via build tags:
  `plugin_sync_unix.go` keeps the real flock implementation (Linux + macOS +
  BSD); `plugin_sync_windows.go` is a permissive noop with a comment
  explaining why (the race we're guarding is rare on a single-user CLI tool
  and the worst case is a redundant idempotent sync). `TestTryAcquireSyncLock_Mutex`
  skips on Windows via `runtime.GOOS`.

## [0.4.7] - 2026-05-10

### Added

- **Plugin drift detection + opt-in auto-sync on SessionStart** — the SessionStart hook now compares the binary's compile-time version against the installed Claude Code plugin cache (`~/.claude/plugins/cache/anchored/anchored/<X.Y.Z>/`) and, when out of sync, injects an `<anchored_plugin_update>` notice into `additionalContext`. When `config.Plugin.AutoUpdate` is true (the default), it also fast-forwards the marketplace git mirror and removes the stale cache dir so Claude Code reinstalls from the updated mirror on its next launch. Closes the v0.4.x gap where the binary auto-updated to a new release but the plugin cache (and therefore the hooks) stayed pinned to whatever was first installed.
- **`config.Plugin` block** — `auto_update` (default true), `marketplace_dir`, and `cache_dir` give users control over the auto-sync paths. Defaults target the canonical Claude Code locations; set `auto_update: false` to receive the manual-fix notice without any side effects.

### Safety

- `git pull --ff-only` is invoked under a 10-second context timeout with `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/true`, `SSH_ASKPASS=/bin/true`, and `GIT_OPTIONAL_LOCKS=0` — an unreachable remote, missing credential, or askpass GUI cannot hang SessionStart.
- Mutating operations are guarded by an advisory flock at `~/.anchored/plugin_sync.lock` (non-blocking). Two Claude Code windows opening simultaneously won't race on the cache dir; the loser skips silently.
- Only the directory matching the currently installed version is removed — never the cache root.
- All failure modes (missing dirs, timeout, divergent history, permission denied) are captured into `SyncError` and surface to the user via the notice; the hook never aborts.

## [0.4.6] - 2026-05-10

### Added

- **VERSION single-source** — new `/VERSION` file is the canonical version. `make build` reads it and injects via `-ldflags -X main.Version=$(cat VERSION)`; `make sync-version` runs `cmd/version-sync` to rewrite `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` from VERSION. Bumping is now `echo X.Y.Z > VERSION && make sync-version` instead of editing five files. The hardcoded version in `cmd/anchored/main.go` becomes a `"dev"` placeholder overridden by ldflags in real builds.
- **Pre-search injection in UserPromptSubmit hook** — when the user prompt mentions memory cues (PT/EN word-bounded triggers: memória, lembra, decidimos, fechamos, remember, settled on, like we discussed, from now on, our, we have/did/use, …), the hook runs a project-scoped BM25 query against `memories_fts` with a 200ms timeout and injects up to 3 hits as `<anchored_search_preview>` in `additionalContext`. The agent now sees relevant memories before deciding whether to call `anchored_search` — making the right answer the path of least resistance.
- **`memory.ListOptions.Categories` SQL filter** — list memories matching multiple categories in one SQL call instead of pulling everything and filtering in Go. `toolContext` now pulls only the durable-knowledge categories (decision/learning/plan/preference/fact) directly, so projects dominated by summary/event still surface enough actionable rows in the L0 bundle.
- **`session_events` retention** — new `Manager.CleanupOldEvents(ctx, retention)` plus a daily goroutine in `serve.go` that drops rows older than 30 days. Without this the table grows by ~1 row per tool call (PostToolUse hook) and never shrinks. First sweep runs on startup, then every 24h until shutdown.
- **PreToolUse hook registered with narrow matcher** — `hooks/hooks.json` now wires `anchored hook pretooluse` for `mcp__anchored__anchored_execute|_execute_file|_batch_execute` only. Substring-based dangerous-pattern detector (`rm -rf /`, `mkfs`, `dd if=/dev/zero`, `:(){:|:&};:`, `curl|sh`) is too coarse for general-purpose Bash but sound for sandbox-execute payloads where the user explicitly asked anchored to run code.

### Changed

- **Lightweight hook DB init** — `hook_posttooluse` and `hook_sessionstart` no longer call `memory.NewService` (which loads ONNX, ~470MB memory map + cold-start cost). New `openHookContext` opens the DB direct + `project.Detector` only — every PostToolUse firing now pays a few milliseconds instead of bootstrapping the full search pipeline.
- **`toolContext` queries run in parallel** — identity read, project meta lookup, project-scoped stats, and recent memories+events run in 4 concurrent goroutines via `sync.WaitGroup`. Slower DBs see ~2-3× faster bundle assembly.
- **`anchored_search` returns structured XML** — hits are now wrapped in `<anchored_search query="…" count="N">…<hit id=… category=… score=… [project=…]>content</hit>…</anchored_search>` instead of a numbered list. LLM agents can integrate fragments directly without reformatting; attributes are XML-escaped.
- **Tighter tool descriptions** — `anchored_context` and `anchored_search` descriptions in `pkg/mcp/tools.go` shrunk to one paragraph each with explicit example triggers (PT-BR + EN). Short imperatives plus concrete examples drive better tool-call rates than long lists.
- **`runHookPostToolUse` split into wiring + core** — new `recordPostToolUseEvent(deps PostToolUseDeps)` takes stdin/stdout/db/resolver as injectable dependencies. Production runs go through `runHookPostToolUse` as before; tests now exercise the full stdin → DB path against an in-memory sqlite without touching `os.Stdin`/`os.Stdout`.

## [0.4.5] - 2026-05-10

### Fixed

- **`anchored_context` actually returns context** — `pkg/mcp/server.go::toolContext` was a stub that always returned the literal string `"No memory context available yet."`, contradicting both its own tool description and the routing block's "MUST CALL FIRST" instruction. It now bundles identity (`~/.anchored/identity.md`, capped at 600 chars), project metadata (id/name/path/memory count/category breakdown), the 5 most recent durable memories scoped to the project (decision/learning/plan/preference/fact), and the 5 most recent priority-≤2 session events. Output is XML-tagged and capped at 4 KB; truncation drops whole lines from the tail and inserts `<truncated/>` to keep the closing tag intact. Falls back to the legacy string only when every section is empty.
- **PostToolUse hook records events again** — `cmd/anchored/hook_posttooluse.go` had three compounding bugs: (a) the INSERT statement listed 9 columns but only supplied 8 values with literals (`'tool_call'`, `3`) misaligned into the wrong slots; (b) it read `--session-id` from a flag that `hooks/hooks.json` never passes; (c) the input struct decoded a `tool` field, but Claude Code sends `tool_name`/`tool_input`/`tool_response`. Net effect: 100% of PostToolUse events were silently dropped. Hook now reads the canonical Claude Code payload from stdin (with flag-based fallback), aligns the SQL exactly (9 cols / 9 values / 6 binds), prefers `tool_response` for the summary (falls back to `tool_input`), and never returns non-zero on init/insert failure — graceful JSON response only, so the upstream tool call is never blocked.
- **`hook pretooluse` reads canonical fields** — same `tool_name`/`tool_input` migration as PostToolUse, with fallback to legacy `tool`/`arguments` for manual scripts. Doc comment now states explicitly that the hook is not registered in `hooks/hooks.json` and that `checkDangerousPattern` is too coarse for general-purpose tool calls.

### Changed

- **`anchored doctor` probes more clients** — added Gemini CLI (`~/.gemini/settings.json`) and VS Code Copilot workspace config (`.vscode/mcp.json`) to the MCP-registration probe set. Existing probes (Claude Code, Cursor, OpenCode) unchanged.

## [0.4.4] - 2026-05-08

### Added

- **Opt-in NDJSON debug log** — new `pkg/debuglog` writes one JSON event per line covering every hook firing (SessionStart, UserPromptSubmit, PostToolUse, PreCompact, PreToolUse) and every MCP message / tool call (with latency, args preview, result preview). Disabled by default; enable via `debug.enabled: true` in `~/.anchored/config.yaml` or `ANCHORED_DEBUG=1` env. Path defaults to `~/.anchored/debug.log` and is owner-only (`0o600`) since events embed prompt heads and tool args. Lets users analyze "did anchored actually fire?" after the fact instead of guessing.
- **Auto-update integrity** — the background self-updater now downloads `checksums.txt` from the release, validates the tarball's SHA-256 against the published digest before swapping the binary, and refuses to install on mismatch. The previous binary is preserved at `<dst>.prev` so a bad update can be rolled back with one `mv`. New unit tests cover format parsing, mismatch rejection (no `.prev` created, no `.new` leaked), and happy-path swap.

### Changed

- **Routing block reframed as intent-based directives** — `<anchored_memory>` in `pkg/mcp/routing.go` and the skill description in `skills/anchored/SKILL.md` no longer enumerate dictionaries of trigger phrases. Replaced with rules ("any mention of memory/memória/lembra/remember", "any reference to past work / 'we' / 'our'", "any architectural recommendation about to be made — search first") plus an explicit `<forbidden>` clause: `NEVER require the user to say a magic phrase before you use memory`. Goal: stop silent bypass when the user phrases a memory request casually or in a language not in the list.
- **Updater error visibility** — release-check failures (network down, repo renamed, asset matrix changed, GitHub rate-limit) now log at `Warn` instead of `Debug`, so users running with default `Info` log level see when their auto-update is broken.

## [0.4.3] - 2026-05-06

### Changed

- **Single source of truth for routing guidance** — the `<anchored_memory>` routing block now lives in `pkg/mcp/routing.go` (`AnchoredRoutingBlock`) and is consumed in two places that used to drift: the MCP `initialize.instructions` field and the SessionStart / UserPromptSubmit hook payloads. Pure-MCP clients (no hook support) and Claude Code-style clients (with `additionalContext`) now receive the same guidance text via different channels — no duplication, no contradictions.
- **Routing block gained `<session_continuity>`** — explicit reminder that decisions/preferences saved via `anchored_save` remain authoritative across sessions and tools, that contradictions should prefer `anchored_update` over duplicates, and that revocations use `anchored_forget`. Mirrors the equivalent section in context-mode's routing block.

## [0.4.2] - 2026-05-06

### Added

- **SessionStart + UserPromptSubmit hooks** — the Claude Code plugin now ships `hooks/hooks.json` wired to `anchored hook sessionstart` (injects the `<anchored_memory>` routing block plus a project-scoped recap of recent decisions/events at conversation start) and `anchored hook userpromptsubmit` (re-injects the routing block on every prompt so the reminder survives compaction). Result: the agent calls `anchored_context` / `anchored_search` / `anchored_save` proactively without the user having to ask.
- **Hook subcommand expansion** — new `anchored hook userpromptsubmit` (Claude Code contract `{hookSpecificOutput:{hookEventName,additionalContext}}`); the existing `anchored hook sessionstart` now emits the same contract instead of an opaque `{resume_context,...}` blob.
- **Cursor + OpenCode sample configs** — `configs/cursor/{mcp.json,hooks.json}` and `configs/opencode/opencode.json` register the `anchored` MCP server and route SessionStart/UserPromptSubmit/PreCompact equivalents to the same hook subcommands. `configs/README.md` walks through install per IDE.

### Changed

- **`kg_query` → `anchored_kg_query` / `kg_add` → `anchored_kg_add`** — namespaced under the `anchored_` prefix so the knowledge-graph tools sit alongside `anchored_search` / `anchored_save` instead of appearing as orphan top-level tools. The legacy `kg_query` / `kg_add` names still dispatch to the same handlers (no breaking change for older clients), but new clients should use the prefixed names. Tool descriptions and the MCP `initialize.instructions` text were updated accordingly.

## [0.4.1] - 2026-05-05

### Added

- **`anchored_fetch_and_index` multi-URL batch** — accepts `requests: [{url, source}, ...]` plus optional `concurrency: 1-8` to fan out HTML→markdown→index across several URLs in one call. Per-URL failures are reported in the response (no abort). Single-URL `url`/`source` form is preserved.
- **`anchored_batch_execute` parallel commands** — optional `concurrency: 1-8` runs sandbox commands in parallel for I/O-bound batches. Result order matches input order regardless of concurrency. Defaults to sequential.
- **`anchored_ctx_search` content-type filter** — optional `content_type: 'code' | 'prose'` narrows hits to source-code chunks or prose chunks. Empty (default) preserves prior behavior.
- **`anchored_ctx_search` progressive throttling** — calls 1-3 return normal `limit`, 4-8 are clamped to 1 result/query with a "fold into batch" warning, 9+ are blocked and redirect to `anchored_batch_execute`. Counter resets whenever `anchored_batch_execute`, `anchored_fetch_and_index`, or `anchored_index` repopulates the corpus.

### Changed

- **Sandbox tool descriptions sharpened for routing** — `anchored_execute`, `anchored_execute_file`, `anchored_batch_execute`, `anchored_ctx_search`, `anchored_fetch_and_index`, and `anchored_index` now lead with explicit "USE INSTEAD OF Bash/Read/WebFetch" guidance and position `anchored_batch_execute` as the primary research tool / `anchored_ctx_search` as the follow-up tool, so models pick the sandbox path even when no external routing hooks are present.

## [0.4.0] - 2026-05-05

### Added

- **Claude Code plugin** — installable via `/plugin marketplace add jholhewres/anchored` and `/plugin install anchored@anchored`. Bundles 6 slash commands (`/anchored:context`, `/anchored:search`, `/anchored:save`, `/anchored:stats`, `/anchored:doctor`, `/anchored:purge`), an auto-triggered skill, and the MCP server registration in one install.
- **`anchored doctor`** — diagnostic checklist: binary version, ONNX model + tokenizer, FTS5/WAL, embedding coverage, MCP registration in Claude Code (`~/.claude.json`), Cursor, OpenCode, identity file. Each failure prints the exact fix command.
- **`anchored purge`** — wipe memories. Default is soft-delete (recoverable for 30 days); `--hard` backs the DB up to `~/.anchored/data/anchored.db.YYYY-MM-DD-HHMMSS.bak` and resets to a fresh schema.
- **Categorizer expansion** — ~25 new bilingual (PT+EN) regex patterns covering `learning` (was previously broken at 6/23K entries), `decision` ("vamos com", "settled on", "going forward"), `preference` ("I always", "minha preferência"), `plan` ("TODO", "next up", "preciso de"), and `event` ("merged", "shipped", "deployed"). Plan now runs before decision so "Next up: refactor" wins over "refactor". Unit tests cover each pattern.

### Changed

- **`anchored_save` requires `category`** — moved from optional to required in the MCP tool inputSchema. The description lists every category with examples so the LLM picks intentionally instead of relying on regex auto-detect. Service-level fallback to `Categorize()` is preserved if a client passes empty (no breaking change for older callers).
- **Tool descriptions rewritten for proactive but discreet usage** — `anchored_context`, `anchored_search`, `anchored_save`, `kg_query`, `kg_add`, `anchored_update`, `anchored_forget` now lead with explicit trigger conditions and examples. `Instructions` field on `initialize` reframed: "use memory silently, don't narrate the search, quality over quantity". Generic across every IDE / AI tool.

### Fixed

- **`anchored_execute_file` was non-functional** — the `path` argument was logged and dropped; user code never received `FILE_PATH` or `FILE_CONTENT`. Now injects a language-specific prelude exposing both variables (JavaScript, TypeScript, Python, Shell, Ruby, Go, Rust, PHP, Perl, R, Elixir).
- **`anchored_execute` env hardening** — host environment is now sanitized before launching the sandbox subprocess: ~40 hijack-prone vars stripped (`LD_PRELOAD`, `BASH_ENV`, `PYTHONSTARTUP`, `RUBYOPT`, `GIT_SSH_COMMAND`, `NODE_OPTIONS`, …) and forced sandbox vars added (`PYTHONUNBUFFERED=1`, `NO_COLOR=1`, `TERM=dumb`).
- **`anchored_fetch_and_index` gains `force` parameter** — bypass the 24h URL cache to re-fetch fresh content. Defaults to false for backward compatibility.
- **`purge --hard` data-safety** — `copyFile` now does explicit `Sync()` + checked `Close()`, removing the partial backup if anything fails. Pre-backup `PRAGMA wal_checkpoint(TRUNCATE)` ensures the `.bak` is self-contained even if another process holds the DB open.
- **Categorizer false positives reduced** — bare `pattern` and `design` keywords removed from the `decision` regex; "design patterns in Python" no longer becomes a decision. Plan-pattern word boundaries tightened.

### Docs

- README setup makes `claude mcp add -s user anchored anchored` explicit. Without `-s user` Claude Code registers at local scope (current project only); user scope makes Anchored available in every project. Also documents that running sessions must be restarted to see newly-added MCP servers.

## [0.3.3] - 2026-05-05

### Added

- **Auto-updater** — `anchored serve` now checks GitHub releases on startup, downloads the matching tarball if a newer version is available, and atomically replaces `~/.anchored/bin/anchored`. The check runs in a background goroutine and never blocks the MCP handshake. Only triggers for binaries living under the canonical install dir, so dev builds are never overwritten. Disable via `ANCHORED_NO_AUTOUPDATE=1`. The currently running process keeps its in-memory image; the new binary activates on the next MCP spawn.

## [0.3.2] - 2026-05-05

### Fixed

- **Claude Code MCP registration** — `anchored init --tool claude-code` now writes to `~/.claude.json` (the actual file Claude Code reads) instead of the non-existent `~/.claude/mcp.json`. Anchored was silently invisible to Claude Code while working in OpenCode/Cursor.
- **Backup before merge** — `registerMCP` now writes a `.bak` copy before overwriting any existing tool config, protecting user state (e.g., the 200KB+ payload Claude Code keeps in `~/.claude.json`).

### Docs

- README setup section corrected: shows `claude mcp add anchored anchored` as the canonical Claude Code install path and clarifies the actual config-file locations for each tool.

## [0.2.0] - 2025-04-30

### Added

- **Vector cache** (T1): in-memory RAM cache of all embedding vectors for sub-millisecond similarity search
- **PreTrainedTokenizerFast** (T3): full HuggingFace `tokenizer.json` support with BPE/WordPiece, normalizer pipeline, and automatic fallback to WordPiece (`vocab.txt`)
- **Model swap** (T4): switch from `all-MiniLM-L6-v2` to `paraphrase-multilingual-MiniLM-L12-v2` with automatic cache migration
- **Embedding cache migration** (T5): lazy re-embedding when model changes, old cache entries auto-invalidated
- **OpenCode importer** (T6): SQL-based import from `opencode.db` (sessions, messages, parts, todos)
- **Cursor rules importer** (T7): `.mdc` file parsing with YAML frontmatter (description, globs)
- **Incremental import tracking** (T8): `imports` table with delta sync per source (mtime, SHA-256, timestamps)
- **Entity detector** (T9): regex-based entity extraction from queries using project/keyword/content snapshots, with cached TTL
- **Topic change detector** (T10): detects conversation topic shifts to trigger broader, more diverse retrieval
- **Essential stories layer** (T11, L1): deterministic per-project summary template (top facts, decisions, events, preferences) with 6h SQLite cache
- **On-demand layer** (T12, L2): entity-driven FTS5 retrieval with category diversification and budget enforcement
- **Stack telemetry** (T21): atomic counters for L0/L1/L2 byte counts, L1 cache hit/miss stats
- **Memory indexer** (T16): heading-aware markdown chunking with SHA-256 delta sync and polling-based file watching
- **KG extractor** (T17): automatic pattern-based entity and relationship extraction on every save, rate-limited to 5 triples
- **Credential redaction** (T18): expanded regex patterns for API keys, tokens, passwords, SSH keys, and generic secrets
- **CLI refactoring** (T19): single `main.go` split into 11 separate command files with shared helpers
- **Config management** (T20): `anchored config show|set` with dotted key support
- **CHANGELOG.md** (T22): this file

### Changed

- Embedding model upgraded from `all-MiniLM-L6-v2` (English-only, 512 tokens) to `paraphrase-multilingual-MiniLM-L12-v2` (50+ languages, 128 tokens, 384 dims)
- Hybrid search now includes entity boost (1.1x) and project boost
- Memory stack L1 replaced generic project layer with deterministic essential stories
- Memory stack L2 now uses entity detection + topic change detection instead of simple regex lookup
- Import pipeline now uses `paraphrase-multilingual-MiniLM-L12-v2` instead of `bge-small-en-v1.5`
- Claude Code importer rewritten with robust multi-line JSON parsing and session summaries
- CLI split from single `main.go` (332 lines) into 11 files (~50-130 lines each)

### Fixed

- Download timeout increased from 5min to 10min for ~470MB model downloads
- HTTP resume support for interrupted model downloads (Range header)
- Duplicate map key compile errors in query expansion stop words
