#!/bin/sh
# agentmesh installer (Linux & macOS).
#
# Default behaviour:
#   - If the binary or any harness config is already present, just download
#     the latest binary in place and exit (fast update path).
#   - Otherwise, install the binary and show an interactive checkbox menu to
#     pick which harness(es) to register with.
#
#   curl -fsSL https://blueheisenberg.github.io/agentmesh/install.sh | sh
#
# Env vars (pass to `sh`, not `curl`):
#   PREFIX           install dir (default: /usr/local/bin or ~/.local/bin)
#   VERSION          release tag (default: latest)
#   REPO             owner/repo  (default: BlueHeisenberg/agentmesh)
#   NAME             explicit node display name; default is auto-derived
#                    from CWD + git branch by the binary itself.
#   HARNESS          comma list: claude,cursor,codex,antigravity,all,none
#                    when set, skip the interactive menu.
#   RECONFIGURE      if set, force the harness picker even when agentmesh is
#                    already registered.
#   SKIP_REGISTER    if set, never touch any harness config.
#   CLAUDE_CONFIG, CURSOR_CONFIG, CODEX_CONFIG, ANTIGRAVITY_CONFIG
#                    override individual config paths.

set -eu

REPO="${REPO:-BlueHeisenberg/agentmesh}"
VERSION="${VERSION:-latest}"
BIN="agentmesh"

# ---- printing (ASCII-only - older Windows terminals & some Linux locales
# mangle Unicode glyphs) ---------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_DIM='\033[2m'; C_BOLD='\033[1m'; C_AMBER='\033[38;5;179m'
  C_GREEN='\033[38;5;108m'; C_RED='\033[38;5;167m'; C_OFF='\033[0m'
else
  C_DIM=''; C_BOLD=''; C_AMBER=''; C_GREEN=''; C_RED=''; C_OFF=''
fi
info()  { printf "${C_AMBER}::${C_OFF} %s\n" "$*"; }
ok()    { printf "${C_GREEN}[ok]${C_OFF} %s\n" "$*"; }
warn()  { printf "${C_AMBER}[!] ${C_OFF}%s\n" "$*"; }
die()   { printf "${C_RED}[x] ${C_OFF}%s\n" "$*" >&2; exit 1; }

# ---- detect OS / arch ----------------------------------------------------

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin) os="darwin" ;;
  linux)  os="linux"  ;;
  msys*|mingw*|cygwin*) die "Windows is supported via the PowerShell installer: iwr -useb https://blueheisenberg.github.io/agentmesh/install.ps1 | iex" ;;
  *) die "Unsupported OS: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "Unsupported arch: $ARCH" ;;
esac

# ---- resolve version -----------------------------------------------------

if [ "$VERSION" = "latest" ]; then
  info "fetching latest release tag for ${REPO}"
  VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep -E '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -n "$VERSION" ] || die "could not resolve latest release"
fi
ver_no_v="$(echo "$VERSION" | sed -E 's/^v//')"

# ---- known harness config paths -----------------------------------------

CLAUDE_CONFIG="${CLAUDE_CONFIG:-${HOME}/.claude.json}"
CURSOR_CONFIG="${CURSOR_CONFIG:-${HOME}/.cursor/mcp.json}"
CODEX_CONFIG="${CODEX_CONFIG:-${HOME}/.codex/config.toml}"
ANTIGRAVITY_CONFIG="${ANTIGRAVITY_CONFIG:-${HOME}/.gemini/antigravity/mcp_config.json}"

# detect_existing populates EXISTING with a space-separated list of harnesses
# that already have an agentmesh entry. Useful for the fast-path "just update
# the binary" flow.
EXISTING=""
detect_existing() {
  EXISTING=""
  if [ -f "$CLAUDE_CONFIG" ] && grep -q '"agentmesh"' "$CLAUDE_CONFIG" 2>/dev/null; then
    EXISTING="$EXISTING claude"
  fi
  if [ -f "$CURSOR_CONFIG" ] && grep -q '"agentmesh"' "$CURSOR_CONFIG" 2>/dev/null; then
    EXISTING="$EXISTING cursor"
  fi
  if [ -f "$CODEX_CONFIG" ] && grep -q '^\[mcp_servers.agentmesh\]' "$CODEX_CONFIG" 2>/dev/null; then
    EXISTING="$EXISTING codex"
  fi
  if [ -f "$ANTIGRAVITY_CONFIG" ] && grep -q '"agentmesh"' "$ANTIGRAVITY_CONFIG" 2>/dev/null; then
    EXISTING="$EXISTING antigravity"
  fi
  EXISTING="$(echo "$EXISTING" | sed -E 's/^ +//;s/ +/ /g')"
}

# ---- pick install dir ----------------------------------------------------

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

# ---- download + verify + install ----------------------------------------

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
[ "$expected" = "$actual" ] || die "checksum mismatch: expected ${expected}, got ${actual}"
ok "checksum ok"

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
  *) warn "${PREFIX} is not in your PATH - add it to your shell rc." ;;
esac

# ===========================================================================
# Harness registration
# ===========================================================================

if [ -n "${SKIP_REGISTER:-}" ]; then
  info "skipping harness registration (SKIP_REGISTER set)"
  exit 0
fi

detect_existing

# Fast path: agentmesh already configured somewhere AND user didn't ask to
# reconfigure. We're done.
if [ -n "$EXISTING" ] && [ -z "${RECONFIGURE:-}" ] && [ -z "${HARNESS:-}" ]; then
  ok "agentmesh already configured for: ${C_BOLD}${EXISTING}${C_OFF}"
  printf "${C_DIM}    re-run with RECONFIGURE=1 ... | sh to add/change harnesses${C_OFF}\n"
  printf "\n${C_BOLD}You're done.${C_OFF} Restart the harness(es) above to pick up v${ver_no_v}.\n"
  exit 0
fi

# ---- decide which harnesses to register with ----------------------------
# Priority:
#   1. $HARNESS env var if set (skip the interactive menu)
#   2. interactive checkbox menu via /dev/tty
#   3. non-interactive fallback (default: claude only)

# checkbox_menu() - POSIX-sh multi-select with arrow keys.
# Args: <prompt> <pre-checked-bitmask> <label1> <label2> ...
#   bitmask is a string of 0/1 chars, same length as #labels. e.g. "1010".
# Outputs to stdout: space-separated 1-based indexes of selected items.
# UI is drawn on /dev/tty so it doesn't pollute the captured output.
checkbox_menu() (
  prompt=$1
  checked=$2
  shift 2

  n=$#
  i=1
  while [ "$i" -le "$n" ]; do
    eval "label_$i=\$$i"
    i=$((i+1))
  done
  cursor=1

  # Snapshot terminal state, switch to raw mode.
  oldstty=$(stty -g </dev/tty)
  stty -icanon -echo -isig </dev/tty
  printf '\033[?25l' >/dev/tty   # hide cursor
  trap '
    printf "\033[?25h" >/dev/tty
    stty "$oldstty" </dev/tty 2>/dev/null
  ' EXIT INT TERM

  # First pass: print header + blank lines reserved for items + hint line.
  printf '%s\n' "$prompt" >/dev/tty
  i=0
  while [ "$i" -lt "$n" ]; do
    printf '\n' >/dev/tty
    i=$((i+1))
  done
  printf '\n' >/dev/tty   # hint line
  total_lines=$((n + 1))

  render() {
    # Move cursor up to the first item line.
    printf '\033[%dA' "$total_lines" >/dev/tty
    i=1
    while [ "$i" -le "$n" ]; do
      bit=$(printf '%s' "$checked" | cut -c"$i")
      mark="[ ]"
      [ "$bit" = "1" ] && mark="[x]"
      arrow="  "
      [ "$i" = "$cursor" ] && arrow="${C_AMBER}> ${C_OFF}"
      label_var="label_$i"
      eval "label=\$$label_var"
      printf '\033[2K%s%s %s\n' "$arrow" "$mark" "$label" >/dev/tty
      i=$((i+1))
    done
    printf '\033[2K%s  arrows: move | space: toggle | enter: confirm | q: cancel%s\n' "$C_DIM" "$C_OFF" >/dev/tty
  }
  render

  while :; do
    key=$(dd bs=1 count=1 </dev/tty 2>/dev/null || true)
    case "$key" in
      ' ')
        # Toggle bit at cursor position.
        head_pos=$((cursor - 1))
        tail_pos=$((cursor + 1))
        if [ "$head_pos" -ge 1 ]; then
          head=$(printf '%s' "$checked" | cut -c1-"$head_pos")
        else
          head=""
        fi
        bit=$(printf '%s' "$checked" | cut -c"$cursor")
        if [ "$tail_pos" -le "$n" ]; then
          tail=$(printf '%s' "$checked" | cut -c"$tail_pos"-)
        else
          tail=""
        fi
        if [ "$bit" = "1" ]; then newbit=0; else newbit=1; fi
        checked="${head}${newbit}${tail}"
        render
        ;;
      $'\n'|'')   # Enter
        break
        ;;
      q|Q)
        checked=$(printf '%s' "$checked" | tr '1' '0')
        break
        ;;
      $'\033')    # ESC - start of an arrow-key sequence
        k2=$(dd bs=1 count=1 </dev/tty 2>/dev/null || true)
        if [ "$k2" = "[" ]; then
          k3=$(dd bs=1 count=1 </dev/tty 2>/dev/null || true)
          case "$k3" in
            A) [ "$cursor" -gt 1 ] && cursor=$((cursor - 1)); render ;;
            B) [ "$cursor" -lt "$n" ] && cursor=$((cursor + 1)); render ;;
          esac
        fi
        ;;
    esac
  done

  printf '\033[?25h' >/dev/tty
  stty "$oldstty" </dev/tty 2>/dev/null

  # Emit selected indexes.
  i=1
  out=""
  while [ "$i" -le "$n" ]; do
    bit=$(printf '%s' "$checked" | cut -c"$i")
    if [ "$bit" = "1" ]; then
      out="$out $i"
    fi
    i=$((i+1))
  done
  printf '%s\n' "$(echo "$out" | sed -E 's/^ +//')"
)

# Build a 4-char initial mask: 1 if that harness is already configured.
mask=""
for h in claude cursor codex antigravity; do
  case " $EXISTING " in
    *" $h "*) mask="${mask}1" ;;
    *)        mask="${mask}0" ;;
  esac
done
# If nothing pre-existing, default-check claude (most common case).
[ "$mask" = "0000" ] && mask="1000"

choose_harnesses() {
  # Sets HARNESS to a space-separated subset of: claude cursor codex antigravity
  if [ -n "${HARNESS:-}" ]; then
    # Translate the env value: comma-separated, "all", "none"
    HARNESS=$(echo "$HARNESS" | tr ',' ' ' | xargs)
    case " $HARNESS " in
      *" all "*)  HARNESS="claude cursor codex antigravity" ;;
      *" none "*) HARNESS="" ;;
    esac
    return
  fi

  # Need an interactive terminal for the checkbox UI.
  if ! exec 3</dev/tty 2>/dev/null; then
    info "non-interactive shell - defaulting to: claude"
    HARNESS="claude"
    return
  fi
  exec 3<&-

  picks=$(checkbox_menu "${C_BOLD}Select harness(es) to register agentmesh with:${C_OFF}" "$mask" \
    "claude code     ${C_DIM}${CLAUDE_CONFIG}${C_OFF}" \
    "cursor          ${C_DIM}${CURSOR_CONFIG}${C_OFF}" \
    "chatgpt codex   ${C_DIM}${CODEX_CONFIG}${C_OFF}" \
    "antigravity     ${C_DIM}${ANTIGRAVITY_CONFIG}${C_OFF}")

  HARNESS=""
  for idx in $picks; do
    case "$idx" in
      1) HARNESS="$HARNESS claude" ;;
      2) HARNESS="$HARNESS cursor" ;;
      3) HARNESS="$HARNESS codex" ;;
      4) HARNESS="$HARNESS antigravity" ;;
    esac
  done
  HARNESS=$(echo "$HARNESS" | xargs)
}

choose_harnesses

if [ -z "$HARNESS" ]; then
  info "no harness selected - binary installed, no MCP registration done."
  printf "${C_DIM}    re-run with RECONFIGURE=1 ... | sh to pick later.${C_OFF}\n"
  exit 0
fi

# ---- registration --------------------------------------------------------

if ! command -v python3 >/dev/null 2>&1; then
  warn "python3 not found - cannot edit harness configs automatically."
  printf "${C_DIM}    Add this snippet to the JSON harnesses manually:${C_OFF}\n\n"
  cat <<EOF
  "mcpServers": {
    "agentmesh": {
      "command": "${PREFIX}/${BIN}"
    }
  }

EOF
  exit 0
fi

bin_path="${PREFIX}/${BIN}"

register_one() {
  kind="$1"; cfg="$2"
  result="$(python3 - "$kind" "$cfg" "$bin_path" "${NAME:-}" <<'PYEOF'
import json, os, shutil, sys
kind, cfg, bin_path, name = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

# Default args list. If NAME is provided, pin it; otherwise let the binary
# auto-derive (CWD + git branch).
args = ["serve"]
if name:
    args.append(f"--name={name}")

if kind == "codex":
    section = "\n[mcp_servers.agentmesh]\n"
    section += f'command = "{bin_path}"\n'
    if args[1:]:
        joined = ", ".join(f'"{a}"' for a in args)
        section += f"args = [{joined}]\n"
    else:
        section += 'args = ["serve"]\n'
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

# JSON kinds.
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

desired = {"command": bin_path, "args": args}
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
    BAD_JSON*) warn "${kind}: ${cfg} isn't valid JSON - not touching it" ;;
    NOT_OBJECT|MCP_NOT_OBJECT)
               warn "${kind}: ${cfg} has an unexpected shape - not touching it" ;;
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
printf "${C_DIM}Each session starts loopback-only. Have the agent call mesh_open_lan to expose it to the LAN.${C_OFF}\n"
printf "${C_DIM}Docs: https://blueheisenberg.github.io/agentmesh/${C_OFF}\n"
