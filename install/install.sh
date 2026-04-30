#!/usr/bin/env bash
set -euo pipefail

VERSION="0.1.0"
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

BINARY_URL="https://github.com/jholhewres/anchored/releases/download/v${VERSION}/anchored-${OS}-${ARCH}"

mkdir -p "$BIN_DIR" "$DATA_DIR"

echo "Installing anchored v${VERSION} to $BIN_DIR..."

curl -fsSL "$BINARY_URL" -o "$BIN_DIR/anchored" || {
    echo "Download failed. Building from source..." >&2
    echo "Install Go 1.24+ and run: go install github.com/jholhewres/anchored/cmd/anchored@v${VERSION}" >&2
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

    if ! grep -q 'alias anchored=' "$rcfile" 2>/dev/null; then
        echo "alias anchored='$HOME/.anchored/bin/anchored'" >> "$rcfile"
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
echo "✓ Anchored v${VERSION} installed to $BIN_DIR/anchored"
echo "✓ Config written to $INSTALL_DIR/config.yaml"
echo ""
echo "Open a new terminal (or run: source ~/.bashrc) then use:"
echo "  anchored serve --stdio"
echo ""
echo "To register with your AI tools:"
echo "  anchored init --tool all"
