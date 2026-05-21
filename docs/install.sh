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

printf "${C_DIM}Add to ~/.claude.json:${C_OFF}\n\n"
cat <<EOF
  "mcpServers": {
    "agentmesh": {
      "command": "${PREFIX}/${BIN}",
      "args": ["serve", "--name=$(hostname -s 2>/dev/null || echo this-machine)"]
    }
  }

EOF
printf "${C_DIM}Then restart your harness. Docs: https://blueheisenberg.github.io/agentmesh/${C_OFF}\n"
