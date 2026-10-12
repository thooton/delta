#!/bin/sh
# Installer for the delta CLI. Downloads the build for this machine from the
# "latest" release and puts it on your PATH. Published to that release as
# install.sh, so the one-liner is:
#
#   curl -fsSL https://github.com/thooton/delta/releases/download/latest/install.sh | sh
#
# It installs to /usr/local/bin when run as root, otherwise ~/.local/bin.
# Set DELTA_INSTALL_DIR to put it somewhere else. If the directory isn't on
# your PATH, it adds it to your shell's startup file.
set -eu

BASE_URL=https://github.com/thooton/delta/releases/download/latest

say() { printf '%s\n' "$*"; }
die() { printf 'delta install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); builds exist for Linux and macOS" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU $(uname -m); builds exist for x86_64 and arm64" ;;
esac

# A shell running under Rosetta reports x86_64 on Apple silicon; use the
# native build there.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
  [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
  arch=arm64
fi

if [ -n "${DELTA_INSTALL_DIR:-}" ]; then
  dir=$DELTA_INSTALL_DIR
elif [ "$(id -u)" = 0 ]; then
  dir=/usr/local/bin
else
  dir=$HOME/.local/bin
fi

url=$BASE_URL/delta-$os-$arch
say "Downloading delta for $os/$arch..."
mkdir -p "$dir"
tmp=$(mktemp "$dir/.delta.XXXXXX")
trap 'rm -f "$tmp"' EXIT
if command -v curl >/dev/null 2>&1; then
  curl -fL --progress-bar -o "$tmp" "$url" || die "download failed: $url"
elif command -v wget >/dev/null 2>&1; then
  wget -q --show-progress -O "$tmp" "$url" || die "download failed: $url"
else
  die "need curl or wget to download $url"
fi
chmod 755 "$tmp"
mv -f "$tmp" "$dir/delta"
trap - EXIT
say "Installed $dir/delta"

case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    case "${SHELL:-}" in
      */zsh) rc=$HOME/.zshrc ;;
      */bash) [ "$os" = darwin ] && rc=$HOME/.bash_profile || rc=$HOME/.bashrc ;;
      */fish) rc=$HOME/.config/fish/config.fish ;;
      *) rc=$HOME/.profile ;;
    esac
    if [ "${rc##*/}" = config.fish ]; then
      line="fish_add_path \"$dir\""
    else
      line="export PATH=\"$dir:\$PATH\""
    fi
    if ! grep -qsF "$line" "$rc"; then
      mkdir -p "$(dirname "$rc")"
      printf '\n# Added by the delta installer\n%s\n' "$line" >>"$rc"
      say "Added $dir to your PATH in $rc"
    fi
    say "Open a new terminal, or run this to use delta now:"
    say "  $line"
    ;;
esac

say "Run 'delta help' to get started."
