# Team Sync — Architecture Plan

Persistent shared memory for dev teams. Self-hosted, open-source, privacy-first.

---

## Problem

Anchored gives each developer a personal, local memory brain. When a team works on the same project, decisions, architectural facts, and learnings live only in each dev's local DB. There is no shared knowledge base — the same question gets answered twice, and decisions made by one dev are invisible to the rest.

## Goal

Extend Anchored with a remote sync layer so that project-scoped knowledge (decisions, facts, learnings, architecture) is shared across team members in real time. Personal data (preferences, style, behavioral metadata) stays local unless the dev explicitly opts in.

The remote product model is the same for self-hosted and cloud: a user creates an account, creates or joins an organization, organizes people into teams, and stores shared project memory at the organization level. Teams control access; projects are owned by the organization.

---

## Architecture Overview

```
Dev A (Claude Code)              Dev B (Cursor)
┌─────────────────┐              ┌─────────────────┐
│ ~/.anchored/     │              │ ~/.anchored/     │
│  anchored.db     │              │  anchored.db     │
│  ┌─────────────┐│              │  ┌─────────────┐│
│  │ LOCAL-ONLY   ││              │  │ LOCAL-ONLY   ││
│  │ preferences  ││              │  │ preferences  ││
│  │ events       ││              │  │ events       ││
│  │ unscoped     ││              │  │ unscoped     ││
│  └─────────────┘│              │  └─────────────┘│
│  ┌─────────────┐│              │  ┌─────────────┐│
│  │ PROJECT X    ││  ←── sync ─→│  │ PROJECT X    ││
│  │ decisions    ││              │  │ decisions    ││
│  │ facts        ││              │  │ facts        ││
│  │ learnings    ││              │  │ learnings    ││
│  │ kg_triples   ││              │  │ kg_triples   ││
│  └─────────────┘│              │  └─────────────┘│
└────────┬────────┘              └────────┬────────┘
         │                                │
         └────────────┬───────────────────┘
                      │
             ┌────────▼────────┐
             │  anchored_oss   │  (team infra)
             │ (Postgres)      │
             │                 │
             │ Project X       │
             │ ├ decisions     │
             │ ├ facts         │
             │ ├ kg_triples    │
             │ └ learnings     │
             └─────────────────┘
```

---

## Two Projects

### [`anchored_oss`](https://github.com/jholhewres/anchored_oss) (sibling repo, independent binary)

Self-hosted/open team sync distribution that teams deploy on their own infrastructure. Acts as the central hub for organization-owned, project-scoped knowledge. Teams run it wherever they run their internal tools. The same protocol/model should be reusable by the future Anchored Cloud service.

### `anchored` (this repo, extension)

The local client gains sync capability. Adds remote config, a background syncer goroutine, and dirty-tracking on the local SQLite DB.

---

## Product Hierarchy

```
Account
└── Organization
    ├── Teams
    │   ├── Members
    │   └── Permissions
    └── Projects
        ├── Shared memories
        ├── Knowledge graph
        ├── Policies / guardrails
        └── Audit log
```

### Account

An account represents a human user. Accounts exist in both Anchored Cloud and self-hosted installs. In self-hosted mode, account creation can be local-only; in cloud mode, it can use email, magic link, GitHub OAuth, or another supported provider.

### Organization

An organization is the top-level ownership boundary. Billing, members, teams, projects, policies, and audit logs belong to the organization.

Projects are organization-level resources, not user-level resources. A user can have personal local memories in `~/.anchored`, but anything remote belongs to an organization project.

### Teams

Teams group organization members and grant access to projects. A user may belong to multiple teams. Team membership decides which remote projects the user's Anchored client can push to or pull from.

### Projects

Projects hold shared memory. Projects can be created manually from the dashboard/CLI or automatically when an AI tool attempts to save project-scoped remote knowledge for a repository that is not registered yet.

Automatic project creation must use a stable, non-personal identifier such as a normalized git remote URL hash, organization slug + repository slug, or explicit remote mapping. It must not use a developer's local filesystem path as the remote project identity.

---

## Sync Rules — What Goes Where

### Push (local → server)

| Category | Pushes? | Condition |
|---|---|---|
| `fact` | Yes | Has `project_id` |
| `decision` | Yes | Has `project_id` |
| `learning` | Yes | Has `project_id` |
| `plan` | Yes | Has `project_id` |
| `summary` | Yes | Has `project_id` |
| `preference` | No | Unless `share_preferences: true` in project config |
| `event` | No | Lifecycle data, stays local |
| No `project_id` | No | Personal/global memories stay local |

### Auto write-through (`auto_sync`)

When the configured remote has `auto_sync` enabled (the default when a remote is configured), `anchored_save` (MCP **and** CLI) pushes each newly-saved memory to the linked remote project automatically — no manual `anchored remote sync` needed. It is **local-first**: the local save always completes synchronously and the remote push is best-effort/async, so a server outage never blocks or fails a save.

Eligibility is decided by the same classification as `remote sync` (`ClassifyForRemote`): only `Syncable` memories leave the machine, and the content is the **redacted** form (home paths → `<user-home>`). User-scoped/personal/secret memories are never pushed. The target is the linked remote project (`remote.projects[0]`), not the local project id.

Configure it in `~/.anchored/config.yaml`:

```yaml
remote:
  enabled: true
  auto_sync: true          # on by default; set false to disable
  server_url: https://memory.yourteam.com
  api_key: anc_live_...
  projects:
    - <remote-project-uuid>
```

### Pull (server → local)

Server returns all non-deleted memories for the project that the client hasn't seen yet (based on watermark). Client upserts into local SQLite with `sync_origin = 'remote'`.

### Privacy by Default

- Memories without `project_id` never leave the machine
- `preference` and `event` categories are blocked at the filter layer
- `access_count`, `last_accessed_at`, and behavioral metadata never sync
- `embedding` vectors are not uploaded — each client embeds locally
- `source_id` is internal metadata, not synced
- Credentials are sanitized by the existing `Sanitizer` before upload
- Local filesystem paths are never sent as remote memory content or project identity
- Personal user data is never accepted from remote tools unless the user explicitly opted in
- Remote write tools must reject content that appears to contain personal paths, home directories, usernames, secrets, or user-local environment details

### Remote Tool Guardrails

Remote tools are stricter than local tools. They may save organization/project knowledge, but they must not leak developer-local context.

Remote writes must block or redact:

- absolute local paths (`/home/alice/...`, `/Users/bob/...`, `C:\Users\bob\...`)
- home-relative paths that identify a user (`~/work/private/...`)
- machine-local runtime details unless explicitly allowed (`localhost` ports, private temp paths, shell history, editor cache paths)
- personal preferences unless `share_preferences` is enabled for that project
- session events, access patterns, and behavioral metadata
- secrets, credentials, tokens, connection strings, private keys

Remote writes should prefer project-relative or repository-relative references:

```text
Good:  pkg/memory/service.go handles memory save orchestration.
Bad:   /home/alice/work/company/private-app/pkg/memory/service.go handles memory save orchestration.
```

The server must enforce these guardrails even if the local client fails to filter correctly. Client-side filtering is convenience; server-side policy is the authority.

---

## Wire Protocol

Single endpoint, bidirectional, JSON over HTTP.

### `POST /v1/sync`

**Request** (client → server):

```json
{
  "project_id": "abc123",
  "client_id": "dev-machine-unique-id",
  "since": "2026-05-20T10:00:00Z",
  "push": [
    {
      "id": "mem_xyz",
      "content": "We use PostgreSQL with schema-per-tenant",
      "category": "decision",
      "source": "claude-code",
      "content_hash": "sha256_abc",
      "keywords": ["postgresql", "schema", "multi-tenant"],
      "created_at": "2026-05-22T14:30:00Z",
      "updated_at": "2026-05-22T14:30:00Z",
      "author": "Dev A"
    }
  ],
  "tombstones": ["mem_old_1"],
  "kg_triples": [
    {
      "subject": "AuthService",
      "predicate": "depends_on",
      "object": "TokenValidator"
    }
  ]
}
```

**Response** (server → client):

```json
{
  "pull": [
    {
      "id": "mem_abc",
      "content": "Auth via JWT with refresh token rotation",
      "category": "decision",
      "source": "cursor",
      "content_hash": "sha256_def",
      "keywords": ["jwt", "auth", "refresh-token"],
      "created_at": "2026-05-21T09:00:00Z",
      "updated_at": "2026-05-21T09:00:00Z",
      "author": "Dev B"
    }
  ],
  "server_tombstones": ["mem_old_3"],
  "kg_triples": [
    {
      "subject": "AuthService",
      "predicate": "integrates_with",
      "object": "UserRepository",
      "confidence": 1.0
    }
  ],
  "watermark": "2026-05-22T15:00:00Z"
}
```

### Conflict Resolution

- **Last-write-wins** based on `updated_at` (UTC, second resolution)
- If client version is newer → server accepts the push
- If server version is newer → client accepts the pull (upsert locally)
- `content_hash` provides idempotency — same content = no-op
- Tombstones (soft deletes) propagate across all nodes

### Sync Cadence

- **Push**: immediate after local save/update/delete (fire-and-forget, background goroutine)
- **Pull**: every 60 seconds (configurable), or on-demand via `anchored remote sync`
- **Offline**: operates normally, marks dirty flags, syncs when back online

---

## Consent Model

```yaml
# ~/.anchored/config.yaml
remote:
  server_url: "https://anchored.mycompany.com"
  api_key: "ak_..."

  projects:
    - project_id: "abc123"
      sync: true                    # enabled for this project
      share_preferences: false      # never share personal prefs (default)
      share_events: false           # never share lifecycle events (default)
      categories:                   # which categories to push (default below)
        - fact
        - decision
        - learning
        - plan
        - summary
```

If a dev wants to share personal preferences for a specific project, they set `share_preferences: true`. Granularity is per-project.

---

## `anchored_oss` Design

### Directory Structure

```
anchored_oss/
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── api/
│   │   ├── sync.go              # POST /v1/sync
│   │   ├── project.go           # CRUD projects
│   │   ├── member.go            # invite, join, roles
│   │   └── middleware.go        # auth, rate limit
│   ├── store/
│   │   ├── postgres.go
│   │   └── migrations/
│   ├── model/
│   │   └── types.go
│   └── config/
│       └── config.go
├── docker-compose.yml
├── Dockerfile
├── Makefile
└── README.md
```

### Postgres Schema

```sql
CREATE TABLE accounts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT UNIQUE NOT NULL,
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    slug        TEXT UNIQUE NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now(),
);

CREATE TABLE teams (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now(),
    UNIQUE(org_id, slug)
);

CREATE TABLE org_members (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'member', -- owner | admin | member
    created_at   TIMESTAMPTZ DEFAULT now(),
    UNIQUE(org_id, account_id)
);

CREATE TABLE team_members (
    team_id      UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (team_id, account_id)
);

CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    remote_key  TEXT NOT NULL, -- stable non-personal project identity (e.g. git remote hash)
    created_by  UUID REFERENCES accounts(id),
    created_at  TIMESTAMPTZ DEFAULT now()
    UNIQUE(org_id, slug),
    UNIQUE(org_id, remote_key)
);

CREATE TABLE team_project_access (
    team_id     UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'writer', -- reader | writer | maintainer
    created_at  TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (team_id, project_id)
);

CREATE TABLE members (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id   UUID REFERENCES projects(id) ON DELETE CASCADE,
    api_key_hash TEXT NOT NULL UNIQUE,
    role         TEXT NOT NULL DEFAULT 'member',  -- admin | member
    display_name TEXT NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE memories (
    id           TEXT PRIMARY KEY,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    category     TEXT NOT NULL,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    keywords     TEXT[],
    source       TEXT,
    author       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    deleted_at   TIMESTAMPTZ,
    metadata     JSONB
);

CREATE INDEX idx_memories_project_updated ON memories(project_id, updated_at);
CREATE UNIQUE INDEX idx_memories_content_hash_project
    ON memories(content_hash, project_id)
    WHERE deleted_at IS NULL;

CREATE TABLE kg_entities (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    aliases     TEXT[],
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE kg_triples (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_id   UUID NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    predicate    TEXT NOT NULL,
    object_id    UUID NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    confidence   REAL DEFAULT 1.0,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    valid_from   TIMESTAMPTZ DEFAULT now(),
    valid_to     TIMESTAMPTZ,
    created_at   TIMESTAMPTZ DEFAULT now()
);
```

The `members` table above is the minimal self-hosted member model. Cloud deployments should use `accounts`, `organizations`, `teams`, `org_members`, `team_members`, and `team_project_access` as the canonical hierarchy.

### Automatic Project Creation

When a local Anchored client tries to push a project-scoped memory and the remote server does not know the project yet, the server may create it automatically if policy allows it.

The client sends a project claim that contains only non-personal identifiers:

```json
{
  "project_claim": {
    "name": "anchored",
    "remote_key": "git:sha256:...",
    "git_host": "github.com",
    "repo_slug": "jholhewres/anchored"
  }
}
```

The client must not send:

```json
{
  "local_path": "/home/alice/company/anchored",
  "home_dir": "/home/alice",
  "username": "alice"
}
```

If the organization allows auto-create, the project is created under the organization and the user's teams receive the default access policy. If auto-create is disabled, the server rejects the push with a clear error so an admin can create/map the project manually.

### Auth

- **No SSO for v1.** API key per team member.
- Admin creates project, generates invite keys.
- Devs redeem invite keys from their local Anchored client.
- API keys are hashed (bcrypt) on the server — only the dev's `config.yaml` holds the plaintext.

### Onboarding Flow

```
1. Team admin deploys `anchored_oss` (docker-compose up)
2. Admin creates project:  POST /v1/projects  (with admin secret)
3. Admin generates invite keys:  POST /v1/projects/{id}/invites
4. Admin shares invite keys with team
5. Dev joins:  anchored remote add https://anchored.company.com --key <invite-key>
   → Anchored saves config, tests connection, performs first pull
```

### Roles

| Role | Can do |
|---|---|
| `admin` | Create/delete project, manage members, purge project memories |
| `member` | Push/pull memories, soft-delete own memories, search |

### Team Project Roles

| Role | Can do |
|---|---|
| `reader` | Pull/search shared memories |
| `writer` | Pull/search/push memories |
| `maintainer` | Manage project policies, delete shared memories, review dream suggestions |

---

## `anchored` (this repo) — Changes

### New Package: `pkg/sync/`

```
pkg/sync/
├── client.go       # HTTP client for the remote server
├── syncer.go       # Background goroutine (push/pull cycle)
├── conflict.go     # Last-write-wins + tombstone handling
├── filter.go       # Decide what syncs based on config + category + scope
└── config.go       # RemoteConfig parsing from config.yaml
```

### Local Schema Changes (SQLite migration)

```sql
-- Migration N+1
ALTER TABLE memories ADD COLUMN sync_dirty BOOLEAN DEFAULT FALSE;
ALTER TABLE memories ADD COLUMN sync_origin TEXT DEFAULT 'local';   -- 'local' | 'remote'

CREATE TABLE IF NOT EXISTS sync_state (
    project_id TEXT PRIMARY KEY,
    watermark  TEXT NOT NULL,
    last_sync  DATETIME,
    client_id  TEXT NOT NULL
);
```

### Service Layer Integration

`Service.Save()` notifies the syncer after a successful local write:

```go
// After persisting to SQLite:
if s.syncer != nil && projectID != nil {
    s.syncer.NotifyDirty(m.ID, projectID)
}
```

### Syncer Goroutine

```go
type Syncer struct {
    client  *Client
    store   Store
    dirtyCh chan dirtyItem
    ticker  *time.Ticker
    logger  *slog.Logger
}

func (s *Syncer) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case item := <-s.dirtyCh:
            s.push(ctx, item)
        case <-s.ticker.C:
            s.pullAll(ctx)
        }
    }
}
```

### Search Transparency

Remote memories are upserted into the local `memories` table (marked `sync_origin = 'remote'`). The existing `HybridSearcher` queries `memories` directly — no changes to the search path. Synced knowledge appears in search results, context injection, and knowledge graph queries as if it were local.

### New CLI Commands

```
anchored remote add <server-url> --key <invite-key>    # Join a project
anchored remote sync                                # Force immediate sync
anchored remote status                              # Show sync state, last watermark
anchored remote list                                # List configured remotes
anchored remote forget <project-id>                 # Leave a project (purge remote data locally)
```

---

## Data Flow — Complete Sequence

```
Dev A saves a decision on project X
    │
    ├── Service.Save()
    │     ├── Sanitize content
    │     ├── Detect category
    │     ├── Detect project (from cwd)
    │     ├── Compute content_hash
    │     ├── Upsert into local SQLite
    │     ├── Embed async (local ONNX)
    │     ├── Extract KG triples (local)
    │     └── Notify syncer (sync_dirty = true)
    │
    ├── Syncer.push()
    │     ├── Filter: category in allowed list? project_id present? → yes
    │     ├── POST /v1/sync { push: [decision], since: watermark }
    │     └── Server upserts into Postgres
    │
    │   (60 seconds later)
    │
    Dev B: Syncer.pull() fires on ticker
    │
    ├── POST /v1/sync { push: [], since: watermark_B }
    │
    ├── Server returns Dev A's decision
    │
    └── Client upserts into local SQLite
          ├── sync_origin = 'remote'
          ├── Embed async (local ONNX)
          ├── KG triples upserted locally
          └── Update watermark

    Dev B searches "auth architecture"
    │
    └── HybridSearcher returns Dev A's decision
        (transparently — no distinction between local and remote memories)
```

---

## Local Sync Readiness Status

Summary of what's been implemented locally and what's still pending for `anchored_oss`.

### Implemented (local side)

| Feature | Location | Notes |
|---|---|---|
| Preference scope metadata | `anchored_save` accepts `scope` (user/project/team) | Defaults to `user` for preferences. Stored in metadata JSON. |
| Sanitizer custom patterns | `NewSanitizer` accepts `SanitizerConfig` | Custom redaction rules beyond built-in secret patterns. |
| Remote safety filter | `pkg/sync/filter.go` `RemoteSafetyFilter` | Detects local paths, secrets, personal preferences in outbound content. |
| Stable project identity | `pkg/project/detector.go` | `RemoteKey` derived from git remote URL (SHA-256). Auto-computes and backfills. Repos without remotes stay local-only. |
| Sync metadata columns | SQLite migration | `sync_dirty`, `sync_origin`, `author`, `remote_project_key` on `memories` table. `sync_state` table added. |
| Service observers | `MemoryObserver` interface | `OnMemorySaved`, `OnMemoryUpdated`, `OnMemoryDeleted` hooks. Non-blocking, optional. |
| List extensions | `ListOptions` | `Source` and `IncludeDeleted` filters. |
| Memory inspect CLI | `anchored inspect <id>` | Full JSON output with all metadata. |
| Memory export CLI | `anchored export` | Filters: --project, --category, --source, --include-deleted. Formats: json, jsonl. Embeddings excluded. |
| Remote config | `RemoteConfig` in config.yaml | `enabled`, `server_url`, `api_key`, `projects`. |
| Remote status CLI | `anchored remote status` | Shows current remote config. Offline. |
| Remote preview CLI | `anchored remote preview` | Classifies memories as syncable/blocked/needs_review. Offline, no network requests. |
| Dream review improvements | `anchored dream --apply <action-id>` | Apply single actions. Dedup actions soft-delete. Contradictions rejected (manual review required). |

### Pending (requires `anchored_oss`)

| Feature | Dependency | Notes |
|---|---|---|
| HTTP sync client (`pkg/sync/client.go`) | `anchored_oss` server endpoint | Push/Pull DTOs and safety-validated payloads in progress. |
| Background syncer goroutine | `anchored_oss` server endpoint | Periodic push/pull cycle, dirty channel, ticker. |
| Conflict resolution | `anchored_oss` server endpoint | Last-write-wins based on `updated_at`. |
| Server deployment | `anchored_oss` repo | Postgres, Docker Compose, API endpoints. |
| Auth (API keys) | `anchored_oss` server | Invite keys, member management. |
| Dashboard | `anchored_oss` server | Web UI for project/member management. |

---

## Decisions Summary

| Decision | Choice | Rationale |
|---|---|---|
| Server database | Postgres | Real multi-writer concurrency, production-grade |
| Server transport | REST (JSON over HTTP) | Simple, curl-friendly, no codegen needed |
| Embeddings on server | Text only | Each client embeds locally with its own ONNX model |
| Knowledge graph sync | Triples sync | Factual project data, not personal |
| Container format | Docker Compose (Postgres + server) | One-command deploy for teams |
| Auth model (v1) | API key per member | Simple, self-hosted, no external identity provider |
| Conflict resolution | Last-write-wins (updated_at) | Simple, predictable, works offline |
| Offline support | Dirty flags + eventual sync | Local-first, sync when online |
| Project ownership | Implicit via `project_id` | Matches existing Anchored scoping model |
| Preference/event sync | Opt-in per project | Privacy by default |
| Embedding sync | Never | Local-only, saves bandwidth, no vector storage on server |
| Two repos vs monorepo | Two repos | Independent release cadence, server is optional |

---

## License / Distribution Direction

The goal is simple:

- companies may use Anchored self-hosted on their own servers for their own teams;
- Anchored Cloud is the official hosted service;
- third parties should not be able to repackage Anchored OSS and sell a competing hosted Anchored service.

There are two simple paths:

```text
Path A — true open source:
  Anchored server core: AGPLv3
  Companies can self-host internally.
  Competitors can host only if they publish their source changes.

Path B — explicit no-resale / no-hosted-competitor:
  Anchored server core: source-available license with a "no managed service" clause
  Companies can self-host internally.
  Third parties cannot sell a hosted Anchored-like service.
  This is not OSI open source, even if the source is public.
```

AGPLv3 is the simplest open-source-aligned option because it allows self-hosting and commercial internal use while requiring service providers who modify and run the server over a network to publish their source changes. It does not fully prevent a clone from existing, but it makes a closed competing hosted version much harder.

If the hard requirement is "companies may run it internally, but nobody may resell/host a competing Anchored service", use Path B. The license language should stay narrow: allow internal business use, allow self-hosting for own teams, forbid offering Anchored or a substantially similar hosted service to third parties.

Avoid calling the project "open source" if choosing a non-commercial or source-available license that forbids commercial use. Those licenses may block resale more directly, but they also prevent some legitimate company usage and are not OSI open source.

The license decision should stay simple and understandable to developers. Default recommendation: AGPLv3 if the OSI open-source label matters more; a narrow "no managed service" source-available license if blocking resale is more important.
