#!/usr/bin/env bash
set -euo pipefail

REPO="jholhewres/anchored"
INSTALL_DIR="$HOME/.anchored"
BIN_DIR="$INSTALL_DIR/bin"
DATA_DIR="$INSTALL_DIR/data"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
RESET='\033[0m'

info()  { echo -e "${CYAN}$1${RESET}"; }
ok()    { echo -e "${GREEN}$1${RESET}"; }
warn()  { echo -e "${YELLOW}$1${RESET}"; }
err()   { echo -e "${RED}$1${RESET}" >&2; }

TTY="/dev/tty"

prompt_yes_no() {
    local msg="$1" default="${2:-Y}"
    local yn
    if [[ "$default" == "Y" ]]; then
        yn="[Y/n]"
    else
        yn="[y/N]"
    fi
    while true; do
        echo -ne "${BOLD}${msg}${RESET} ${yn} " > "$TTY"
        read -r answer < "$TTY"
        answer="${answer:-$default}"
        case "$answer" in
            [Yy]*) return 0 ;;
            [Nn]*) return 1 ;;
            *) ;;
        esac
    done
}

ARCH=$(uname -m)
case "$(uname -s)" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)      err "Unsupported OS: $(uname -s)"; exit 1 ;;
esac

case "$ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)              err "Unsupported arch: $ARCH"; exit 1 ;;
esac

LATEST=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | sed 's/.*"v\(.*\)".*/\1/')
if [ -z "$LATEST" ]; then
    err "Failed to determine latest version"; exit 1
fi

ARCHIVE="anchored_${LATEST}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/v${LATEST}/${ARCHIVE}"

mkdir -p "$BIN_DIR" "$DATA_DIR"

info "Installing anchored v${LATEST} (${OS}/${ARCH})..."
curl -fsSL "$URL" | tar xz -C "$BIN_DIR" anchored || {
    err "Download failed."
    echo "Install Go 1.24+ and run: git clone https://github.com/${REPO}.git && cd anchored && make build" >&2
    exit 1
}

chmod +x "$BIN_DIR/anchored"

if ! echo "$PATH" | grep -q "$BIN_DIR"; then
    for rc in .bashrc .zshrc .profile .bash_profile; do
        rcfile="$HOME/$rc"
        [ -f "$rcfile" ] || continue
        if ! grep -q 'anchored/bin' "$rcfile" 2>/dev/null; then
            echo "" >> "$rcfile"
            echo "# Anchored memory server" >> "$rcfile"
            echo 'export PATH="$HOME/.anchored/bin:$PATH"' >> "$rcfile"
        fi
    done
fi

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

ok "anchored v${LATEST} installed to $BIN_DIR/anchored"
echo ""

if ! prompt_yes_no "Import memories from existing tools?"; then
    echo ""
    info "Open a new terminal (or run: source ~/.bashrc)"
    echo ""
    info "Add to your MCP config:"
    echo '  { "mcpServers": { "anchored": { "command": "anchored" } } }'
    exit 0
fi

echo ""

CLAUDE_PROJECTS="$HOME/.claude/projects"
DEVCLAW_DB="$HOME/Workspace/private/devclaw/data/memory.db"

detected=()

if [ -d "$CLAUDE_PROJECTS" ]; then
    count=$(find "$CLAUDE_PROJECTS" -name "*.jsonl" 2>/dev/null | wc -l)
    if [ "$count" -gt 0 ]; then
        detected+=("claude-code|$count sessions|$CLAUDE_PROJECTS")
    fi
fi

if [ -f "$DEVCLAW_DB" ]; then
    chunks=$(sqlite3 "$DEVCLAW_DB" "SELECT COUNT(*) FROM chunks;" 2>/dev/null || echo "0")
    if [ "$chunks" -gt 0 ]; then
        detected+=("devclaw|$chunks chunks|$DEVCLAW_DB")
    fi
fi

CURSOR_RULES="$HOME/.cursor/rules"
if [ -d "$CURSOR_RULES" ]; then
    mdc_count=$(find "$CURSOR_RULES" -name "*.mdc" 2>/dev/null | wc -l)
    if [ "$mdc_count" -gt 0 ]; then
        detected+=("cursor|$mdc_count rules|$CURSOR_RULES")
    fi
fi

OPENCODE_JSON="$HOME/opencode.json"
if [ -f "$OPENCODE_JSON" ] || [ -d "$HOME/sessions" ]; then
    detected+=("opencode|detected|$HOME")
fi

if [ ${#detected[@]} -eq 0 ]; then
    warn "No memory sources detected."
    exit 0
fi

info "Detected memory sources:"
echo ""
for i in "${!detected[@]}"; do
    IFS='|' read -r name detail path <<< "${detected[$i]}"
    printf "  %d. ${BOLD}%s${RESET} — %s\n" "$((i+1))" "$name" "$detail"
done
echo ""

echo -ne "${BOLD}Which sources to import?${RESET} (comma-separated numbers, or 'all'): " > "$TTY"
read -r selection < "$TTY"

if [[ "$selection" == "all" || "$selection" == "a" ]]; then
    sources=("claude-code" "devclaw" "cursor" "opencode")
else
    sources=()
    IFS=',' read -ra nums <<< "$selection"
    for n in "${nums[@]}"; do
        n=$(echo "$n" | tr -d ' ')
        idx=$((n-1))
        if [ "$idx" -ge 0 ] && [ "$idx" -lt ${#detected[@]} ]; then
            IFS='|' read -r name _ _ <<< "${detected[$idx]}"
            sources+=("$name")
        fi
    done
fi

if [ ${#sources[@]} -eq 0 ]; then
    warn "Nothing selected."
    exit 0
fi

echo ""
info "Importing from: ${sources[*]}..."
echo ""

"$BIN_DIR/anchored" import "${sources[@]}" 2>&1 | while IFS= read -r line; do
    case "$line" in
        *level=WARN*)  warn "$line" ;;
        *level=ERROR*) err "$line" ;;
        *)             echo "$line" ;;
    esac
done

echo ""
ok "Done!"
echo ""
info "Open a new terminal (or run: source ~/.bashrc)"
echo ""
info "Add to your MCP config:"
echo '  { "mcpServers": { "anchored": { "command": "anchored" } } }'
