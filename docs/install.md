# Install and update

From GitHub Releases (Linux, macOS, and Windows under Git Bash, MSYS2 or Cygwin):

```bash
curl -fsSL https://raw.githubusercontent.com/jholhewres/anchored/main/install/install.sh | bash
```

From source:

```bash
git clone https://github.com/jholhewres/anchored.git
cd anchored
make build
sudo cp bin/anchored /usr/local/bin/
```

First run creates `~/.anchored/` and downloads the local embedding model when needed (~470 MB).

## Updating

```bash
anchored self-update             # install the latest release; exits 0 if already current
anchored self-update --check     # report only; exits 10 when an update is available
anchored self-update --version v0.17.0   # pin to a release (an older one needs --force)
anchored self-update --force --yes       # install past a refusal, no prompt
anchored self-update --json      # machine-readable result; branch on .action
anchored self-update --no-plugin # leave the Claude Code plugin alone
```

The download is verified against the release checksums before anything is
replaced, and the previous binary is kept at `<bin>.prev`, so one rename undoes
an update. Restart your MCP clients afterwards — a running server holds the old
binary until it exits.

Self-update targets `~/.anchored/bin`. Two installs are deliberately refused
there, and both refusals name themselves and hand you the override:

- **A binary built from a checkout**, because overwriting it would revert your
  own work to the release tag. `--force` replaces it and asks first.
- **A binary anywhere else**, including the `/usr/local/bin` copy the
  install-from-source recipe above makes. Only the canonical install is managed
  automatically, so updating that copy in place needs
  `anchored self-update --force` (and, since the directory is root-owned,
  likely `sudo` — the refusal prints the exact command).

`anchored doctor` reports when a release is available.

### macOS and Windows: one manual reinstall first

Self-update is new, and the binary that performs an update is the one already
installed — so the fix for a platform cannot reach an install that predates it.
Releases up to and including v0.18.0 shipped a background updater that resolves
no digest on macOS (the darwin archives are absent from `checksums.txt`, and
that build cannot read the `.sha256` sidecar) and no archive at all on Windows
(it looks only for `.tar.gz`, and Windows publishes `.zip`).

On Linux nothing is needed: an existing install picks up the next release on its
own. On macOS and Windows, download the current release once and replace the
binary in `~/.anchored/bin` by hand. Every update after that one is in-place.

Every published archive resolves and verifies: the linux and windows archives
are digested in `checksums.txt`, and the darwin archives — built on a macOS
runner, since CGO with FTS5 cannot cross-compile — carry a `.sha256` sidecar
the updater falls back to when checksums.txt does not list the asset. The swap
itself is platform-specific: unix backs the old binary up by hardlink so the
path never disappears, while windows renames it aside first, because the image
loader will not let a running `.exe` be replaced in place. The windows path is
covered by tests but has not been exercised on real hardware.

Self-update trusts GitHub's release infrastructure. The checksum it verifies
protects the download from corruption and tampering in transit; it does not
prove who built the binary.
