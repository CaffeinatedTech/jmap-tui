#!/usr/bin/env bash
# jmap-tui installer — downloads the latest release binary for this platform.
#
#   curl -fsSL https://raw.githubusercontent.com/CaffeinatedTech/jmap-tui/master/install.sh | bash
#
# Installs to ~/.local/bin by default. Override with JMAP_TUI_INSTALL_DIR.
# Assets are matched by OS/arch inside the latest GitHub release, so the
# goreleaser archive name template only has to contain the OS and the Go arch
# (e.g. jmap-tui_0.1.0_linux_amd64.tar.gz).
set -euo pipefail

REPO="CaffeinatedTech/jmap-tui"
BIN="jmap-tui"
INSTALL_DIR="${JMAP_TUI_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar >/dev/null 2>&1 || die "tar is required"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch="(amd64|x86_64|x64)" ;;
	aarch64 | arm64) arch="(arm64|aarch64)" ;;
	*) die "unsupported architecture: $arch" ;;
esac
case "$os" in
	linux) os="linux" ;;
	darwin) os="(darwin|macos|osx)" ;;
	*) die "unsupported OS: $os — download a binary from https://github.com/$REPO/releases or build from source" ;;
esac

say "-> checking the latest release of $REPO"
json=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest") ||
	die "could not reach GitHub (offline, rate-limited, or no release published yet)"

pick() { printf '%s\n' "$json" | grep -o 'https://[^"]*' | grep -Ei "$os" | grep -Ei "$arch" | grep -Ei "$1" | head -n 1; }
url=$(pick '\.(tar\.gz|tgz)$')
[ -n "$url" ] || url=$(pick '\.zip$')
[ -n "$url" ] || die "no binary for this OS/architecture in the latest release — build from source (see the README)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "-> downloading ${url##*/}"
curl -fsSL "$url" -o "$tmp/pkg"

case "$url" in
	*.zip)
		command -v unzip >/dev/null 2>&1 || die "unzip is required"
		unzip -q "$tmp/pkg" -d "$tmp"
		;;
	*) tar -xzf "$tmp/pkg" -C "$tmp" ;;
esac

bin=$(find "$tmp" -type f -name "$BIN" | head -n 1)
[ -n "$bin" ] || bin=$(find "$tmp" -type f -perm -u+x | head -n 1)
[ -n "$bin" ] || die "archive did not contain an executable"

mkdir -p "$INSTALL_DIR"
cp "$bin" "$INSTALL_DIR/$BIN"
chmod 755 "$INSTALL_DIR/$BIN"

case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*) say "-> note: add $INSTALL_DIR to your PATH, e.g. in ~/.bashrc:"; say "     export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
esac

say "-> installed:"
"$INSTALL_DIR/$BIN" version || true
say "Run 'jmap-tui' to add your first account."
