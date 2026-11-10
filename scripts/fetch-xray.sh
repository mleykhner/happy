#!/bin/sh
set -eu
arch=${1:-amd64}
out=${2:-./bin}
case "$arch" in
 amd64) archive=Xray-linux-64.zip ;;
 arm64) archive=Xray-linux-arm64-v8a.zip ;;
 darwin-arm64) archive=Xray-macos-arm64-v8a.zip ;;
 *) echo 'Supported: amd64, arm64, darwin-arm64' >&2; exit 1 ;;
esac
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
curl --fail --location --proto '=https' --tlsv1.2 "https://github.com/XTLS/Xray-core/releases/download/v26.11.9/$archive" -o "$tmp/$archive"
expected=$(awk -v name="$archive" '$2 == name {print $1}' "$root/packaging/xray-checksums.txt")
actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
[ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo 'Xray checksum mismatch' >&2; exit 1; }
mkdir -p "$out"
unzip -p "$tmp/$archive" xray > "$out/xray"
chmod 755 "$out/xray"
unzip -p "$tmp/$archive" LICENSE > "$out/XRAY-LICENSE"
