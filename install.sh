#!/usr/bin/env bash
# install.sh — bootstrapping installer for git-feedback
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sdougbrown/git-feedback/main/install.sh | bash
#
# Downloads the latest release binary for your platform from GitHub Releases
# and installs it to ~/.local/bin (or a directory of your choice via BINDIR).

set -euo pipefail

OWNER="sdougbrown"
REPO="git-feedback"
BINDIR="${BINDIR:-${HOME}/.local/bin}"

# --- helpers --------------------------------------------------------------

err()   { printf 'install: %s\n' "$*" >&2; exit 1; }
info()  { printf '  %s\n' "$*"; }

# --- detect platform ------------------------------------------------------

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Darwin) OS="darwin"  ;;
  Linux)  OS="linux"   ;;
  *) err "unsupported OS: $OS" ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) err "unsupported architecture: $ARCH" ;;
esac

# Rosetta 2: uname -m reports x86_64 when the shell runs under emulation,
# so an Apple Silicon user would get the amd64 build. Prefer native arm64.
if [ "$OS" = "darwin" ] && [ "$ARCH" = "amd64" ]; then
  if [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ] \
    || [ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = "1" ]; then
    ARCH="arm64"
  fi
fi

info "platform: ${OS}/${ARCH}"

# --- resolve latest release tag -------------------------------------------

printf '  resolving latest release...\n'
payload="$(curl -fsSL "https://api.github.com/repos/${OWNER}/${REPO}/releases/latest")" \
  || err "could not fetch the latest release"
# Match the key explicitly so the leftmost match wins regardless of whether
# the API response is pretty-printed or minified single-line JSON.
TAG="$(printf '%s' "$payload" | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"

if [ -z "$TAG" ]; then
  err "could not determine latest release tag"
fi

info "latest release: ${TAG}"

# The tag is interpolated into download URLs; constrain it to a semver-ish
# release tag so a tampered API response cannot alter the request path.
printf '%s' "$TAG" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$' \
  || err "unexpected tag format: $TAG"

# --- download archive -----------------------------------------------------

# goreleaser archive naming: git-feedback_darwin_arm64.tar.gz, git-feedback_linux_amd64.tar.gz
ASSET="git-feedback_${OS}_${ARCH}.tar.gz"
URL="https://github.com/${OWNER}/${REPO}/releases/download/${TAG}/${ASSET}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

printf '  downloading %s...\n' "$ASSET"
curl -fsSL -o "${TMPDIR}/${ASSET}" "$URL" \
  || err "download failed: $URL"

# --- verify checksum -------------------------------------------------------

# The release publishes a checksums.txt (see .goreleaser.yaml); verify the
# archive against it before extraction so a corrupted or tampered download
# fails closed.
printf '  verifying checksum...\n'
curl -fsSL -o "${TMPDIR}/checksums.txt" \
  "https://github.com/${OWNER}/${REPO}/releases/download/${TAG}/checksums.txt" \
  || err "download failed: checksums.txt"
expected="$(grep " ${ASSET}$" "${TMPDIR}/checksums.txt" | head -1 | awk '{print $1}')"
[ -n "$expected" ] || err "no checksum published for ${ASSET}"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "${TMPDIR}/${ASSET}" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "${TMPDIR}/${ASSET}" | awk '{print $1}')"
else
  err "no sha256 utility available (need sha256sum or shasum)"
fi
[ "$actual" = "$expected" ] || err "checksum mismatch for ${ASSET}"

# --- extract & install ----------------------------------------------------

printf '  extracting...\n'
tar -xzf "${TMPDIR}/${ASSET}" -C "$TMPDIR"

mkdir -p "$BINDIR"
install -m 0755 "${TMPDIR}/git-feedback" "${BINDIR}/git-feedback"

info "installed git-feedback ${TAG} → ${BINDIR}/git-feedback"

# --- path hint ------------------------------------------------------------

case ":${PATH}:" in
  *":${BINDIR}:"*)
    info "ready: git-feedback --help"
    ;;
  *)
    printf '\n'
    printf '  Add %s to your PATH to use git-feedback:\n' "$BINDIR"
    printf '    export PATH="%s:$PATH"\n' "$BINDIR"
    printf '\n'
    ;;
esac
