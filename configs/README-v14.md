# Deep host plugins (v0.14)

`anchored init --tool <host>` registers anchored as an MCP server AND, for hosts
with a memory-plugin slot, installs a deep plugin that bridges the agent
lifecycle to the local `anchored` binary (recall via `anchored search`, capture
via `anchored save`) — no REST daemon, unlike agentmemory.

| Host | MCP config | Deep plugin installed to |
|------|-----------|--------------------------|
| openclaw | `~/.openclaw/openclaw.json` (`mcpServers`) | `~/.openclaw/extensions/anchored/` + `plugins.slots.memory=anchored` |
| hermes | `~/.hermes/config.yaml` (`mcp_servers`) | `~/.hermes/plugins/anchored/` + `memory.provider=anchored` |
| pi | — (no MCP) | `~/.pi/agent/extensions/anchored/index.ts` + `settings.json` extensions[] |
| devclaw / gatorclaw / supergator | `~/.<host>/config.yaml` (`mcp.servers[]`) | — (MCP stdio only) |

The canonical plugin bodies live as Go constants in `cmd/anchored/init_plugins.go`
(the source of truth, with `<BIN>` substituted at install time). The files under
`configs/openclaw`, `configs/hermes`, `configs/pi` mirror them for reference.
