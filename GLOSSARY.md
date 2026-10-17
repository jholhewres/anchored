# Glossary

Terms used across the client, the OSS server and their shared contract. Some (the hub as MCP host, the proxy, the Retriever, the write path) name parts of the v0.21 design that are still being built.

- **Client**: the `anchored` binary on a developer's machine: local store, hooks, CLI and MCP server.
- **Instance**: one deployment of Anchored OSS, identified by its `instance_id`. A personal instance and a company instance are two instances.
- **Remote**: the client's configured link to one instance, with its own credential and routing rules.
- **Hub**: the single long-lived local process that owns the store, the model and every background routine, and hosts MCP for all local clients.
- **Proxy**: what `anchored serve --stdio` becomes once the hub hosts MCP: it relays one IDE's stdio session to the hub and holds no store, model or routine of its own.
- **Routine**: work that runs without a request (queue worker, curation, outbox delivery, cache warm, upkeep). Each routine has exactly one owner.
- **Retriever**: the one component that ranks memories for every reader: MCP tools, hooks and CLI.
- **Write path**: the one way a memory is created or changed: sanitize, dedup, persist, enqueue jobs and outbox.
- **Scope**: who a memory is for: `user`, `project`, `team` or `org`. One field; `user` never leaves the machine unless a remote opts in.
- **Pin**: a flag that keeps a memory in context regardless of rank or age.
- **Skill**: a `SKILL.md` directory served over the MCP Skills extension (`io.modelcontextprotocol/skills`). Anchored's own skills are the single source of its instructions; an instance may serve org skills too.
- **Capabilities**: the document an instance publishes at `GET /v1/capabilities`; the client reads features from it instead of inferring them from errors.
- **Grant**: what an OAuth login creates on an instance for one client: org, projects and scopes, revocable from the instance.
- **Legacy key**: an `anc_live_` API key, still accepted where OAuth is not available.
- **Routing key**: the key a repository is known by on a remote, derived from its git origin URL: the canonical form, plus the older v1 form servers may still hold.
- **Path glob**: a pattern in a remote's `paths`. A repository it matches reads from and writes to that remote only.
