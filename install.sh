#!/bin/sh
# Install cmemlan, a self-hosted LAN sync hub for claude-mem.
#
# Please read this before running it. The safer form is:
#
#   curl -fsSL https://raw.githubusercontent.com/andrhamm/claude-mem-lan-sync/main/install.sh -o install.sh
#   less install.sh
#   sh install.sh
#
# Piping straight into a shell means trusting the server at the moment of
# execution, and the checksums it serves alongside the binary cannot help with
# that — they come from the same place.

set -eu

REPO="andrhamm/claude-mem-lan-sync"
BIN="cmemlan"
INSTALL_DIR="${CMEMLAN_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${CMEMLAN_VERSION:-latest}"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }
need curl
need tar

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac
case "$os" in
  linux|darwin) ;;
  *) die "unsupported operating system: $os (on Windows, download the zip from the releases page)" ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || die "could not determine the latest version"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

archive="${BIN}_${VERSION#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

say "downloading $archive ($VERSION)"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

# Verify before unpacking. This catches corruption and a truncated download; it
# is not a defence against a compromised release page, which is what the cosign
# signature on checksums.txt is for. Verification instructions are in the
# release notes.
say "verifying checksum"
if command -v sha256sum >/dev/null 2>&1; then
  ( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - ) ||
    die "checksum verification failed"
elif command -v shasum >/dev/null 2>&1; then
  ( cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c - ) ||
    die "checksum verification failed"
else
  die "no sha256sum or shasum available to verify the download"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$INSTALL_DIR"
install -m 0755 "$tmp/$BIN" "$INSTALL_DIR/$BIN"

# macOS quarantines downloaded binaries, and ours are unsigned and unnotarized.
if [ "$os" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
  xattr -d com.apple.quarantine "$INSTALL_DIR/$BIN" 2>/dev/null || true
fi

say "installed $INSTALL_DIR/$BIN"

# If a service is already installed, the new binary does nothing until restart.
if command -v systemctl >/dev/null 2>&1 &&
   systemctl --user list-unit-files cmemlan.service >/dev/null 2>&1; then
  say "restarting the cmemlan service"
  systemctl --user restart cmemlan.service || say "could not restart automatically; run: systemctl --user restart cmemlan"
fi

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say ""; say "add $INSTALL_DIR to your PATH to use cmemlan" ;;
esac

say ""
say "next: cmemlan serve --bind <lan-address>:8787 --install-service"
say "      cmemlan pair"
