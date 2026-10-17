# Remotes

Anchored works fully offline. A remote is an Anchored OSS instance, personal
or your company's, that receives the project memories safe to share and the
knowledge-graph triples, and answers searches and skills for those projects.
The local store stays complete either way.

## Add a remote

```bash
# the default remote
anchored remote configure --server https://anchored.example.com --key <key>
# a second one, by name
anchored remote configure --name company --server https://anchored.acme.com --key <key>
anchored remote status
```

The keys live in `~/.anchored/config.yaml`, readable by you only (0600). The
same remotes in the config:

```yaml
remotes:
  personal:
    server_url: https://anchored.example.com
    api_key: <key>
    default: true
  company:
    server_url: https://anchored.acme.com
    api_key: <key>
    paths:
      - /home/me/work/*
```

## How a repository picks its remote

1. **`--remote NAME`** on a command wins.
2. **A path glob** in a remote's `paths` that matches the repository names its
   remote outright: searches, skills and writes go to that remote only, even
   if another remote lists a project for the same repository. The glob is
   matched against the repository's root, so its subdirectories, a symlink to
   it and its linked worktrees route the same way; directories above a
   repository's root never count. Between overlapping globs, the one with the
   most literal characters wins, then the remote's name.
3. **Otherwise** the default remote is asked first. If it does not know the
   repository, the other remotes are asked, and the one that lists a project
   under the repository's routing key (derived from its git origin) is used.
4. **A write never passes over a remote that may own the repository.** If that
   remote does not answer, or refuses the credentials, the write is held and
   retried a minute later; a refusal asks you to log in again.

`anchored remote status` shows where the current repository routes and why,
and warns about two remotes marked default or a registration its glob ignores.

## What syncs

```bash
anchored remote preview
```

The preview runs offline and lists what would be sent. Blocked by default:
secrets, local paths, preferences and events, anything in your personal
(`user`) scope, episodic and operational memories, and low-quality ones.

## Sync

```bash
anchored remote sync              # the current repository
anchored remote sync --dry-run    # what it would send, no network
anchored remote sync --all        # every local project, each to its own remote project
anchored remote sync-per-project --min-memories 5
```

Saves made through MCP are queued and delivered in the background to a remote
with `auto_sync` (on unless set to `false`). `--all` and `sync-per-project`
exit 1 when a project was held or failed, so a script can tell.

The server side, the API and its storage are documented with
[Anchored OSS](https://github.com/jholhewres/anchored_oss).
