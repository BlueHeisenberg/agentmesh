#!/bin/sh
# agentmesh installer — fetches the latest release for your OS/arch,
# verifies the sha256 checksum, and drops the binary into $PREFIX.
#
#   curl -fsSL https://blueheisenberg.github.io/agentmesh/install.sh | sh
#
# Env vars:
#   PREFIX     install directory  (default: /usr/local/bin, falls back to ~/.local/bin)
#   VERSION    release tag        (default: latest)
#   REPO       owner/repo         (default: BlueHeisenberg/agentmesh)

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
die()   { printf "${C_RED}✗${C_OFF} %s\n" "$*" >&2; exit 1; }

# ---- detect OS / arch ----------------------------------------------------

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in
  darwin) os="darwin" ;;
  linux)  os="linux"  ;;
  msys*|mingw*|cygwin*) die "Windows is supported but not via this script — download the .zip from the release page." ;;
  *) die "Unsupported OS: $OS" ;;
esac

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) arch="amd64" ;;
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

# release archives use the numeric version (no leading v) per GoReleaser default
ver_no_v="$(echo "$VERSION" | sed -E 's/^v//')"

# ---- pick install dir -----------------------------------------------------

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
SUDO="${SUDO:-}"

# ---- download + verify ----------------------------------------------------

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

archive="agentmesh_${ver_no_v}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${VERSION}"

info "downloading ${C_BOLD}${archive}${C_OFF}${C_DIM} (${VERSION})${C_OFF}"
curl -fsSL --proto '=https' -o "${tmp}/${archive}"        "${base}/${archive}" \
  || die "download failed: ${base}/${archive}"
curl -fsSL --proto '=https' -o "${tmp}/checksums.txt"    "${base}/checksums.txt" \
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

[ "$expected" = "$actual" ] \
  || die "checksum mismatch — expected ${expected}, got ${actual}"
ok "checksum ok"

# ---- extract + install ---------------------------------------------------

tar -xzf "${tmp}/${archive}" -C "$tmp" "$BIN"
chmod +x "${tmp}/${BIN}"

info "installing to ${C_BOLD}${PREFIX}/${BIN}${C_OFF}"
${SUDO} install -m 0755 "${tmp}/${BIN}" "${PREFIX}/${BIN}" \
  || die "install failed (try setting PREFIX=\$HOME/.local/bin)"

ok "installed ${BIN} ${VERSION}"

# ---- next steps ----------------------------------------------------------

printf "\n"
"${PREFIX}/${BIN}" whoami 2>/dev/null \
  | sed "s/^/${C_DIM}    /;s/\$/${C_OFF}/" \
  || true

printf "\n"
case ":$PATH:" in
  *":${PREFIX}:"*) ;;
  *) printf "${C_AMBER}!${C_OFF} ${PREFIX} is not in your PATH — add it to your shell rc.\n" ;;
esac

# ---- auto-register with Claude Code ----------------------------------------
# Adds an "agentmesh" entry to ~/.claude.json under mcpServers, so any future
# Claude Code session on this machine has the mesh tools available with no
# manual setup. Set SKIP_REGISTER=1 to disable. Set NAME=foo to override.

short_host="${NAME:-$(hostname -s 2>/dev/null || echo this-machine)}"
bin_path="${PREFIX}/${BIN}"
ccfg="${CLAUDE_CONFIG:-${HOME}/.claude.json}"

if [ -n "${SKIP_REGISTER:-}" ]; then
  info "skipping Claude Code registration (SKIP_REGISTER set)"
elif ! command -v python3 >/dev/null 2>&1; then
  info "python3 not found — skipping auto-registration"
  printf "${C_DIM}  Add this to ${ccfg} manually:${C_OFF}\n\n"
  cat <<EOF
  "mcpServers": {
    "agentmesh": {
      "command": "${bin_path}",
      "args": ["serve", "--name=${short_host}"]
    }
  }

EOF
else
  result="$(python3 - "$ccfg" "$bin_path" "$short_host" <<'PYEOF'
import json, os, shutil, sys
cfg, bin_path, hostname = sys.argv[1], sys.argv[2], sys.argv[3]

data = {}
if os.path.exists(cfg):
    try:
        with open(cfg) as f:
            data = json.load(f)
    except Exception as e:
        print(f"BAD_JSON {e}")
        sys.exit(2)

if not isinstance(data, dict):
    print("NOT_OBJECT")
    sys.exit(3)

mcp = data.setdefault("mcpServers", {})
if not isinstance(mcp, dict):
    print("MCP_NOT_OBJECT")
    sys.exit(4)

desired = {"command": bin_path, "args": ["serve", f"--name={hostname}"]}
if mcp.get("agentmesh") == desired:
    print("ALREADY")
    sys.exit(0)

if os.path.exists(cfg):
    shutil.copyfile(cfg, cfg + ".bak")
mcp["agentmesh"] = desired
tmp = cfg + ".tmp"
with open(tmp, "w") as f:
    json.dump(data, f, indent=2)
os.replace(tmp, cfg)
print("OK" if os.path.exists(cfg + ".bak") else "CREATED")
PYEOF
)"

  case "$result" in
    OK)        ok "registered with Claude Code in ${ccfg} (backup at ${ccfg}.bak)" ;;
    CREATED)   ok "created ${ccfg} with agentmesh entry" ;;
    ALREADY)   ok "Claude Code already configured for agentmesh" ;;
    BAD_JSON*) printf "${C_AMBER}!${C_OFF} ${ccfg} isn't valid JSON, not touching it. Add the snippet below manually.\n" ;;
    NOT_OBJECT|MCP_NOT_OBJECT)
               printf "${C_AMBER}!${C_OFF} ${ccfg} has an unexpected shape. Add the snippet below manually.\n" ;;
    *)         printf "${C_AMBER}!${C_OFF} couldn't auto-register: %s\n" "$result" ;;
  esac
fi

printf "\n${C_BOLD}You're done.${C_OFF} Open any Claude Code session and try ${C_AMBER}mesh_whoami${C_OFF}.\n"
printf "${C_DIM}Docs: https://blueheisenberg.github.io/agentmesh/${C_OFF}\n"
