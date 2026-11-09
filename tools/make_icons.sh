#!/bin/sh
# Regenerates the PWA icons in icons/ from favicon.svg (rounded, transparent corners:
# purpose "any") and icons/maskable.svg (full bleed, logo inside the 80 % safe zone:
# purpose "maskable", Apple touch icon). Needs Chromium (headless screenshot).
set -eu
cd "$(dirname "$0")/.."
chrome=$(command -v chromium || command -v chromium-browser || command -v google-chrome)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
shot() { # svg size out
  printf '<!doctype html><style>html,body{margin:0;background:transparent}img{display:block;width:%spx;height:%spx}</style><img src="%s">' "$2" "$2" "$PWD/$1" > "$tmp/p.html"
  "$chrome" --headless=new --no-first-run --disable-gpu --user-data-dir="$tmp/prof" --hide-scrollbars --default-background-color=00000000 \
    --allow-file-access-from-files --window-size="$2,$2" --screenshot="$PWD/$3" "file://$tmp/p.html" >/dev/null 2>&1
}
shot favicon.svg 192 icons/icon-192.png
shot favicon.svg 512 icons/icon-512.png
shot icons/maskable.svg 512 icons/maskable-512.png
shot icons/maskable.svg 180 icons/apple-touch-icon.png
file icons/*.png
