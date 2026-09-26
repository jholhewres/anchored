# Contributing to Anchored

Thanks for your interest. Here's how to contribute effectively.

## Development

```bash
git clone https://github.com/jholhewres/anchored.git
cd anchored
make build
make test
make eval
```

**Requirements**: Go 1.25+ and a C compiler (CGO builds SQLite).

FTS5 is enabled by go-sqlite3's `sqlite_fts5` build tag, so plain `go` commands
need it too: `go test -tags sqlite_fts5 ./...`. Do not enable FTS5 through
`CGO_CFLAGS`: setting it replaces Go's default `-O2` and compiles SQLite
unoptimised.

## Making Changes

1. **Small, focused PRs** — one concern per PR. Mixed refactor + feature PRs are hard to review.
2. **Match existing patterns** — read a few files in the package you're modifying before writing code. Follow the naming, error handling, and structure you see.
3. **No comment sprawl** — code should be self-documenting. Comments are for security invariants, complex algorithms, regex patterns, and public API contracts.
4. **Tests for new behavior** — at minimum, test the happy path and one edge case. Place test files next to the implementation (`foo.go` → `foo_test.go`).
5. **No type suppression** — never use unchecked type assertions (`x := iface.(T)` without the two-value form), bare error ignores (`_ = someFunc()`), or empty `recover()` without handling.

## Commit Style

Semantic prefixes, lowercase, imperative mood:

```
feat(memory): add lifecycle metadata v2
fix(search): apply lifecycle boost before temporal decay
chore: upgrade Go 1.25
docs: update tool support table
```

## Pull Requests

- Include a description of **what changed** and **why**.
- Reference issues when applicable (`Closes #12`).
- CI runs, and a PR must pass: `go test -tags sqlite_fts5 ./...` on Linux and
  macOS, `make eval`, `go run ./cmd/version-sync --check`, and golangci-lint on
  the lines the PR changes (`golangci-lint run --new-from-rev=origin/main`).

## Adding Tool Support

Anchored supports MCP-compatible AI coding tools. To add a new tool:

1. **`cmd/anchored/init_cmd.go`** — add the tool to `parseToolFlag`, `isToolInstalled`, `getToolMCPPath`. If the tool uses a non-standard config format (e.g., TOML, different root key), add it to `getToolMCPConfig` and create a registration function.
2. **`cmd/anchored/doctor.go`** — add a probe to `checkMCPRegistration` and any custom detection logic (e.g., `hasVSCodeAnchoredEntry`).
3. **`README.md`** — add the tool to the setup table with its config file path.
4. **Importer** (optional) — if the tool stores local data, add an importer in `pkg/importer/`.

## Reporting Issues

- Include: Anchored version (`anchored --version`), OS, tool + version, and steps to reproduce.
- For MCP registration issues, run `anchored doctor` and include the output.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
