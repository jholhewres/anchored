# Anchored

> Persistent cross-tool memory for AI coding agents. Single binary. Zero dependencies.

Anchored is an MCP memory server that gives your AI coding tools a shared, persistent brain. Install once, and Claude Code, Cursor, OpenCode, and any MCP-compatible tool can read, write, and search the same knowledge base.

## Development

```bash
# Build
make build

# Test
make test

# Run locally
./bin/anchored serve --stdio
```

## Conventions

- Commit messages, tag annotations and CHANGELOG entries are written in English, following Conventional Commits (`type(scope): subject`), regardless of the language used in the conversation.
- Build and test with the `sqlite_fts5` tag (`make build`, `make test`, or `go test -tags sqlite_fts5 ./...`).

## Project Structure

See [docs-anchored/](../docs-anchored/) for design documents and architecture planning.

## License

[MIT](LICENSE)
