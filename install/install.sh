#!/usr/bin/env bash
set -euo pipefail

REPO="jholhewres/anchored"
INSTALL_DIR="$HOME/.anchored"
BIN_DIR="$INSTALL_DIR/bin"
DATA_DIR="$INSTALL_DIR/data"

ARCH=$(uname -m)
case "$(uname -s)" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)      echo "Unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)              echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed 's/.*"v\(.*\)".*/\1/')
if [ -z "$LATEST" ]; then
    echo "Failed to determine latest version" >&2; exit 1
fi

ARCHIVE="anchored_${LATEST}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${LATEST}/${ARCHIVE}"

mkdir -p "$BIN_DIR" "$DATA_DIR"

echo "Installing anchored v${LATEST} (${OS}/${ARCH})..."
curl -fsSL "$URL" | tar xz -C "$BIN_DIR" anchored || {
    echo "Download failed." >&2
    echo "Install Go 1.24+ and run: git clone https://github.com/${REPO}.git && cd anchored && make build" >&2
    exit 1
}

chmod +x "$BIN_DIR/anchored"

for rc in .bashrc .zshrc .profile .bash_profile; do
    rcfile="$HOME/$rc"
    [ -f "$rcfile" ] || continue
    if ! grep -q 'anchored/bin' "$rcfile" 2>/dev/null; then
        echo "" >> "$rcfile"
        echo "# Anchored memory server" >> "$rcfile"
        echo 'export PATH="$HOME/.anchored/bin:$PATH"' >> "$rcfile"
    fi
done

cat > "$INSTALL_DIR/config.yaml" <<'EOF'
memory:
  storage_dir: ~/.anchored/data
  database_path: ~/.anchored/data/anchored.db
embedding:
  provider: onnx
  model: paraphrase-multilingual-MiniLM-L12-v2
  model_dir: ~/.anchored/data/onnx
  quantize: true
  dimensions: 384
search:
  vector_weight: 0.7
  bm25_weight: 0.3
  max_results: 20
  mmr_enabled: true
  mmr_lambda: 0.7
  temporal_decay_enabled: true
  temporal_decay_half_life_days: 30
sanitizer:
  enabled: true
stack:
  budget_bytes: 3600
  l1_cache_ttl: 6h
  l2_max_results: 5
EOF

echo ""
echo "Installed anchored v${LATEST} to $BIN_DIR/anchored"
echo "Config written to $INSTALL_DIR/config.yaml"
echo ""
echo "Open a new terminal (or run: source ~/.bashrc)"
echo ""
echo "Add to your MCP config:"
echo '  { "mcpServers": { "anchored": { "command": "anchored" } } }'
