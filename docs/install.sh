#!/bin/sh
# agentmesh installer (Linux & macOS) — fetches the latest release for your
# OS/arch, verifies its sha256, installs the binary, and registers agentmesh
# with one or more MCP-speaking harnesses (Claude Code / Cursor / Codex /
# Antigravity).
#
#   curl -fsSL https://blueheisenberg.github.io/agentmesh/install.sh | sh
#
# Env vars (pass to `sh`, not `curl`):
#   PREFIX           install dir          (default: /usr/local/bin or ~/.local/bin)
#   VERSION          release tag          (default: latest)
#   REPO             owner/repo           (default: BlueHeisenberg/agentmesh)
#   NAME             node display name    (default: hostname -s)
#   HARNESS          comma list of:       claude,cursor,codex,antigravity,all,none
#                    (default: interactive prompt; if not interactive: claude)
#   SKIP_REGISTER    if set, skip all harness registration
#   CLAUDE_CONFIG    override the path used for the claude registration
#   CURSOR_CONFIG    override the path used for the cursor registration
#   CODEX_CONFIG     override the path used for the codex registration
#   ANTIGRAVITY_CONFIG  override the antigravity config path

set -eu

REPO="${REPO:-BlueHeisenberg/agentmesh}"
VERSION="${VERSION:-latest}"
BIN="agentmesh"

# ---- pretty printing ------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_DIM='\033[2m'; C_BOLD='\033[1m'; C_AMBER='\033[38;5;179m'
  C_GREEN='\033[38;5;108m'; C_RED='\033[38;5;167m'; C_OFF='\033[0m'
else
  C_DIM=''; C_BOLD=''; C_AMBER=''; C_GREEN=''; C_RED=''; C_OFF=''
fi
info()  { printf "${C_AMBER}::${C_OFF} %s\n" "$*"; }
ok()    { printf "${C_GREEN}✓${C_OFF} %s\n" "$*"; }
warn()  { printf "${C_AMBER}!${C_OFF} %s\n" "$*"; }
die()   { printf "${C_RED}✗${C_OFF} %s\n" "$*" >&2; exit 1; }

# ---- detect OS / arch ----------------------------------------------------

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin) os="darwin" ;;
  linux)  os="linux"  ;;
  msys*|mingw*|cygwin*) die "Windows is supported via the PowerShell installer:\n   iwr -useb https://blueheisenberg.github.io/agentmesh/install.ps1 | iex" ;;
  *) die "Unsupported OS: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "Unsupported arch: $ARCH" ;;
esac

# ---- resolve version ------------------------------------------------------

if [ "$VERSION" = "latest" ]; then
  info "fetching latest release tag for ${REPO}"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -E '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -n "$VERSION" ] || die "could not resolve latest release (is there one published?)"
fi
ver_no_v="$(echo "$VERSION" | sed -E 's/^v//')"

# ---- pick install dir -----------------------------------------------------

SUDO=""
if [ -z "${PREFIX:-}" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then
    PREFIX="/usr/local/bin"
  elif command -v sudo >/dev/null 2>&1 && [ -d /usr/local/bin ]; then
    PREFIX="/usr/local/bin"
    SUDO="sudo"
  else
    PREFIX="${HOME}/.local/bin"
    mkdir -p "$PREFIX"
  fi
fi

# ---- download + verify ----------------------------------------------------

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

archive="agentmesh_${ver_no_v}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${VERSION}"

info "downloading ${C_BOLD}${archive}${C_OFF}${C_DIM} (${VERSION})${C_OFF}"
curl -fsSL --proto '=https' -o "${tmp}/${archive}"     "${base}/${archive}" \
  || die "download failed: ${base}/${archive}"
curl -fsSL --proto '=https' -o "${tmp}/checksums.txt"  "${base}/checksums.txt" \
  || die "checksum download failed"

info "verifying sha256"
expected="$(grep " ${archive}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
[ -n "$expected" ] || die "no checksum entry for ${archive}"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${tmp}/${archive}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${tmp}/${archive}" | awk '{print $1}')"
else
  die "neither sha256sum nor shasum found"
fi
[ "$expected" = "$actual" ] || die "checksum mismatch — expected ${expected}, got ${actual}"
ok "checksum ok"

# ---- extract + install ---------------------------------------------------

tar -xzf "${tmp}/${archive}" -C "$tmp" "$BIN"
chmod +x "${tmp}/${BIN}"
info "installing to ${C_BOLD}${PREFIX}/${BIN}${C_OFF}"
${SUDO} install -m 0755 "${tmp}/${BIN}" "${PREFIX}/${BIN}" \
  || die "install failed (try setting PREFIX=\$HOME/.local/bin)"
ok "installed ${BIN} ${VERSION}"

printf "\n"
"${PREFIX}/${BIN}" whoami 2>/dev/null \
  | sed "s/^/${C_DIM}    /;s/\$/${C_OFF}/" \
  || true
printf "\n"

case ":$PATH:" in
  *":${PREFIX}:"*) ;;
  *) warn "${PREFIX} is not in your PATH — add it to your shell rc." ;;
esac

# ===========================================================================
# Multi-harness registration
# ===========================================================================

if [ -n "${SKIP_REGISTER:-}" ]; then
  info "skipping harness registration (SKIP_REGISTER set)"
  printf "\n${C_BOLD}Binary is installed.${C_OFF} To wire it up later, see https://blueheisenberg.github.io/agentmesh/\n"
  exit 0
fi

short_host="${NAME:-$(hostname -s 2>/dev/null || echo this-machine)}"
bin_path="${PREFIX}/${BIN}"

CLAUDE_CONFIG="${CLAUDE_CONFIG:-${HOME}/.claude.json}"
CURSOR_CONFIG="${CURSOR_CONFIG:-${HOME}/.cursor/mcp.json}"
CODEX_CONFIG="${CODEX_CONFIG:-${HOME}/.codex/config.toml}"
ANTIGRAVITY_CONFIG="${ANTIGRAVITY_CONFIG:-${HOME}/.gemini/antigravity/mcp_config.json}"

# Pick harnesses ------------------------------------------------------------

if [ -z "${HARNESS:-}" ]; then
  # Try to open /dev/tty for read on fd 3 — succeeds only when there's an
  # attached controlling terminal we can actually prompt on.
  if exec 3</dev/tty 2>/dev/null; then
    printf "\n${C_BOLD}Where should I register agentmesh?${C_OFF}\n"
    printf "  ${C_AMBER}1${C_OFF}) claude code     ${C_DIM}%s${C_OFF}\n"   "$CLAUDE_CONFIG"
    printf "  ${C_AMBER}2${C_OFF}) cursor          ${C_DIM}%s${C_OFF}\n"   "$CURSOR_CONFIG"
    printf "  ${C_AMBER}3${C_OFF}) chatgpt codex   ${C_DIM}%s${C_OFF}\n"   "$CODEX_CONFIG"
    printf "  ${C_AMBER}4${C_OFF}) antigravity     ${C_DIM}%s${C_OFF}\n"   "$ANTIGRAVITY_CONFIG"
    printf "  ${C_AMBER}5${C_OFF}) all of the above\n"
    printf "  ${C_AMBER}6${C_OFF}) none — I'll do it manually\n"
    printf "${C_DIM}choose (comma-separated, e.g. 1,2) [default: 1]: ${C_OFF}"
    read -r choice <&3 || choice=""
    exec 3<&-
    [ -z "$choice" ] && choice="1"

    HARNESS=""
    OLDIFS="$IFS"; IFS=','
    for c in $choice; do
      c="$(echo "$c" | tr -d '[:space:]')"
      case "$c" in
        1) HARNESS="$HARNESS claude" ;;
        2) HARNESS="$HARNESS cursor" ;;
        3) HARNESS="$HARNESS codex"  ;;
        4) HARNESS="$HARNESS antigravity" ;;
        5) HARNESS="claude cursor codex antigravity"; break ;;
        6) HARNESS="none"; break ;;
        *) warn "ignoring invalid choice: $c" ;;
      esac
    done
    IFS="$OLDIFS"
  else
    info "non-interactive shell — defaulting to claude (override with HARNESS=...)"
    HARNESS="claude"
  fi
fi
HARNESS="$(echo "$HARNESS" | tr ',' ' ' | xargs)"

if [ "$HARNESS" = "none" ] || [ -z "$HARNESS" ]; then
  info "no harness registration requested"
  printf "\n${C_BOLD}Binary is installed.${C_OFF} See https://blueheisenberg.github.io/agentmesh/ for manual config snippets.\n"
  exit 0
fi

# Registration helper -------------------------------------------------------

if ! command -v python3 >/dev/null 2>&1; then
  warn "python3 not found — cannot edit harness configs automatically"
  printf "${C_DIM}  Snippet to paste manually (JSON harnesses):${C_OFF}\n\n"
  cat <<EOF
  "mcpServers": {
    "agentmesh": {
      "command": "${bin_path}",
      "args": ["serve", "--name=${short_host}"]
    }
  }

EOF
  exit 0
fi

register_one() {
  kind="$1"; cfg="$2"
  result="$(python3 - "$kind" "$cfg" "$bin_path" "$short_host" <<'PYEOF'
import json, os, shutil, sys
kind, cfg, bin_path, hostname = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

entry_args = ["serve", f"--name={hostname}"]

if kind == "codex":
    section = (
        "\n[mcp_servers.agentmesh]\n"
        f'command = "{bin_path}"\n'
        f'args = ["serve", "--name={hostname}"]\n'
    )
    if not os.path.exists(cfg):
        os.makedirs(os.path.dirname(cfg) or ".", exist_ok=True)
        with open(cfg, "w") as f:
            f.write(section.lstrip("\n"))
        print("CREATED"); sys.exit(0)

    text = open(cfg).read()
    if "[mcp_servers.agentmesh]" in text:
        print("ALREADY"); sys.exit(0)
    shutil.copyfile(cfg, cfg + ".bak")
    with open(cfg, "a") as f:
        if not text.endswith("\n"):
            f.write("\n")
        f.write(section)
    print("OK"); sys.exit(0)

# JSON kinds (claude, cursor, antigravity) — all share the mcpServers schema.
data = {}
if os.path.exists(cfg):
    try:
        with open(cfg) as f:
            data = json.load(f)
    except Exception as e:
        print(f"BAD_JSON {e}"); sys.exit(2)

if not isinstance(data, dict):
    print("NOT_OBJECT"); sys.exit(3)

mcp = data.setdefault("mcpServers", {})
if not isinstance(mcp, dict):
    print("MCP_NOT_OBJECT"); sys.exit(4)

desired = {"command": bin_path, "args": entry_args}
if mcp.get("agentmesh") == desired:
    print("ALREADY"); sys.exit(0)

existed = os.path.exists(cfg)
if existed:
    shutil.copyfile(cfg, cfg + ".bak")
else:
    os.makedirs(os.path.dirname(cfg) or ".", exist_ok=True)

mcp["agentmesh"] = desired
tmp = cfg + ".tmp"
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
os.replace(tmp, cfg)
print("OK" if existed else "CREATED")
PYEOF
)"
  case "$result" in
    OK)        ok "${kind}: registered in ${cfg} (backup at ${cfg}.bak)" ;;
    CREATED)   ok "${kind}: created ${cfg} with agentmesh entry" ;;
    ALREADY)   ok "${kind}: already configured" ;;
    BAD_JSON*) warn "${kind}: ${cfg} isn't valid JSON — not touching it" ;;
    NOT_OBJECT|MCP_NOT_OBJECT)
               warn "${kind}: ${cfg} has an unexpected shape — not touching it" ;;
    *)         warn "${kind}: ${result}" ;;
  esac
}

printf "\n"
for h in $HARNESS; do
  case "$h" in
    claude)      register_one claude      "$CLAUDE_CONFIG"      ;;
    cursor)      register_one cursor      "$CURSOR_CONFIG"      ;;
    codex)       register_one codex       "$CODEX_CONFIG"       ;;
    antigravity) register_one antigravity "$ANTIGRAVITY_CONFIG" ;;
    *) warn "unknown harness: $h (allowed: claude, cursor, codex, antigravity)" ;;
  esac
done

printf "\n${C_BOLD}You're done.${C_OFF} Restart the harness(es) you registered with, then try ${C_AMBER}mesh_whoami${C_OFF}.\n"
printf "${C_DIM}Docs: https://blueheisenberg.github.io/agentmesh/${C_OFF}\n"
