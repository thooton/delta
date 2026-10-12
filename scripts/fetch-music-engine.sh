#!/usr/bin/env bash
# Puts the download engine behind `delta music` into cmd/delta/music-engine/,
# where the next `go build ./cmd/delta` embeds it. The engine is an unmodified
# release of sockseek (https://github.com/fiso64/sockseek, AGPL-3.0), pinned
# below by version and checksum. To move to a newer release, change VERSION
# and the checksums (sha256sum of each release asset).
#
# Usage: scripts/fetch-music-engine.sh <goos> <goarch>
#
# Platforms without a release (linux/arm64: sockseek only ships 32-bit ARM)
# get no engine, and `delta music` there says it isn't available.
set -euo pipefail

VERSION=3.0.5
declare -A ASSETS=(
  [linux/amd64]="linux-x64 d0a1e909297bc4aa0e497bfdd7203884945a78adfb3c0477c8f22830ac951b66"
  [darwin/amd64]="osx-x64 dee6bbed20d2538dde7f4a854fc2ac400ad4a9adc5d75c2711aeecf37e7af1f5"
  [darwin/arm64]="osx-arm64 6a18078b78f98a9797d723925574996a7bf87f6cc3012aee9efa1e9977a0edd0"
)

if [ $# -ne 2 ]; then
  echo "usage: $0 <goos> <goarch>" >&2
  exit 2
fi
dir="$(cd "$(dirname "$0")/.." && pwd)/cmd/delta/music-engine"
rm -f "$dir/engine.tar.gz" "$dir/engine.version"

entry="${ASSETS[$1/$2]:-}"
if [ -z "$entry" ]; then
  echo "no music engine for $1/$2; building without one" >&2
  exit 0
fi
read -r rid sum <<<"$entry"
name="sockseek_${VERSION}_${rid}.tar.gz"
cache="${MUSIC_ENGINE_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/delta-build}"
mkdir -p "$cache"
if ! echo "$sum  $cache/$name" | sha256sum -c --status 2>/dev/null; then
  curl -fsSL -o "$cache/$name.part" "https://github.com/fiso64/sockseek/releases/download/v${VERSION}/${name}"
  echo "$sum  $cache/$name.part" | sha256sum -c --quiet
  mv "$cache/$name.part" "$cache/$name"
fi
cp "$cache/$name" "$dir/engine.tar.gz"
echo "$VERSION $sum" >"$dir/engine.version"
echo "music engine $VERSION ($rid) ready for $1/$2" >&2
