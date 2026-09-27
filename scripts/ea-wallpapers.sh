#!/usr/bin/env bash
# Rebuild the Execution Associates backdrops: the site's coast plates cropped
# to 16:10 and stamped with the EXA monogram, bottom right.
#
# Why the crop and the odd-looking mark position: lasso paints a backdrop with
# background-size: cover anchored TOP-LEFT, so a screen narrower than the image
# loses its right edge and a wider one loses its bottom. The plates are 2.33:1,
# which on a ~1.4:1 window hid the right 40% (sun included). Cropped to 16:10
# on the sun side, and with the mark's corner at 82%/85% of the frame, the mark
# stays on screen from ~1.37:1 up to 16:9.
#
# Sources are Stephan's own checkouts; needs ImageMagick 7 (mise).
set -euo pipefail
plates="${EA_PLATES:-$HOME/projects/execution-associates/beta/site/assets/plates}"
mark_src="${EA_MARK:-$HOME/projects/execution-associates/design-iteration/site/img/exa-mark-white-alpha.png}"
out="$(cd "$(dirname "$0")/.." && pwd)/src/web/public/wallpapers/execution-associates"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The mark: 76px tall, 85% white, over a faint halo in the brand's neon glow.
magick "$mark_src" -trim +repage -resize x76 -channel A -evaluate multiply 0.85 +channel "$tmp/mark.png"
magick "$tmp/mark.png" -fill '#F099BF' -colorize 100 -channel A -evaluate multiply 0.5 +channel \
  -background none -gravity center -extent 140x140 -blur 0x9 "$tmp/glow.png"
magick "$tmp/glow.png" "$tmp/mark.png" -gravity center -composite "$tmp/stamp.png"
mw=$(magick identify -format %w "$tmp/mark.png")
mh=$(magick identify -format %h "$tmp/mark.png")

# 16:10 window at full plate height (1613x1008), right-aligned to keep the sun.
# Mark's bottom-right corner lands at (1330, 860); the stamp is centred on it.
x=$((1330 - mw / 2 - 70))
y=$((860 - mh / 2 - 70))
stamp() { # <plate> <id>
  magick "$plates/$1" -crop 1613x1008+739+0 +repage "$tmp/stamp.png" \
    -gravity NorthWest -geometry "+$x+$y" -composite "$tmp/$2.png"
  magick "$tmp/$2.png" -strip -resize '1920x1920>' -define webp:method=6 -quality 82 "$out/$2.webp"
  magick "$tmp/$2.png" -strip -resize '320x320>' -define webp:method=6 -quality 75 "$out/thumbs/$2.webp"
}
stamp day-full.webp 01-golden-hour
stamp night.webp 02-night
