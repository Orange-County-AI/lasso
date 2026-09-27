#!/usr/bin/env bash
# Rebuild the Execution Associates backdrops: the site's coast plates cropped
# to 16:10 and stamped with the EXA monogram near the top left.
#
# Why there: lasso paints a backdrop with background-size: cover anchored
# TOP-LEFT, and the terminal iframe paints its own copy pinned to ITS top-left,
# so the top-left is the one corner every screen size keeps — a narrower frame
# loses the right edge, a wider one the bottom. The mark is inset past herdr's
# sidebar (~255 CSS px, i.e. ~240-370 image px depending on frame height), so
# it lands in the terminal's open space just right of it.
#
# The 16:10 crop on the sun side keeps the sun in frame on more screens: the
# plates are 2.33:1 and lost their right 40% on a ~1.4:1 window.
#
# Sources are Stephan's own checkouts; needs ImageMagick 7 (mise).
set -euo pipefail
plates="${EA_PLATES:-$HOME/projects/execution-associates/beta/site/assets/plates}"
mark_src="${EA_MARK:-$HOME/projects/execution-associates/design-iteration/site/img/exa-mark-white-alpha.png}"
out="$(cd "$(dirname "$0")/.." && pwd)/src/web/public/wallpapers/execution-associates"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The mark: 84px tall, 85% white, over a faint halo in the brand's neon glow.
magick "$mark_src" -trim +repage -resize x84 -channel A -evaluate multiply 0.85 +channel "$tmp/mark.png"
magick "$tmp/mark.png" -fill '#F099BF' -colorize 100 -channel A -evaluate multiply 0.5 +channel \
  -background none -gravity center -extent 140x140 -blur 0x9 "$tmp/glow.png"
magick "$tmp/glow.png" "$tmp/mark.png" -gravity center -composite "$tmp/stamp.png"
mw=$(magick identify -format %w "$tmp/mark.png")
mh=$(magick identify -format %h "$tmp/mark.png")

# 16:10 window at full plate height (1613x1008), right-aligned to keep the sun.
# The mark's top-left corner lands at (265, 90), measured to sit just past the
# sidebar's edge (~245 image px) on a ~1.04:1 terminal; the stamp is centred on it.
x=$((265 + mw / 2 - 70))
y=$((90 + mh / 2 - 70))
stamp() { # <plate> <id>
  magick "$plates/$1" -crop 1613x1008+739+0 +repage "$tmp/stamp.png" \
    -gravity NorthWest -geometry "+$x+$y" -composite "$tmp/$2.png"
  magick "$tmp/$2.png" -strip -resize '1920x1920>' -define webp:method=6 -quality 82 "$out/$2.webp"
  magick "$tmp/$2.png" -strip -resize '320x320>' -define webp:method=6 -quality 75 "$out/thumbs/$2.webp"
}
stamp day-full.webp 01-golden-hour
stamp night.webp 02-night
